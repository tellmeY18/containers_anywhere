package recoveryapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type pruneKube struct {
	t                                                                *testing.T
	snapshot, content                                                bool
	namespaceGone, consumer, pendingSnapshot, failPatch, raceContent bool
	newerGone                                                        bool
	contentUID, policy, rv                                           string
	deletingSnapshot                                                 bool
	actions                                                          []string
}

func pruneFixture(t *testing.T) (*Controller, *pruneKube, Record) {
	t.Helper()
	f := &pruneKube{t: t, snapshot: true, content: true, contentUID: "content-uid", policy: "Retain", rv: "1"}
	at := instant("2026-11-09T00:00:00Z")
	r := Record{UID: "capture-uid", Kind: "RecoveryPoint", Name: "point", Namespace: "example", NamespaceUID: "ns-uid", Phase: "Succeeded", CompletedAt: &at, RetentionEligible: true, Members: []Member{{PVC: "data", UID: "pvc-uid", VolumeName: "pv-data", VolumeHandle: "volume-data", Snapshot: "snapshot-data", SnapshotUID: "snapshot-uid", Content: "content-data", ContentUID: "content-uid", SnapshotHandle: "handle-data", Ready: true}}}
	s := specFixture()
	r.SpecHash, _ = canonicalHash(raw(PointSpec{Capture: &s}))
	c := &Controller{Dir: t.TempDir(), Run: f.run, Prune: func(context.Context, Record) error { f.actions = append(f.actions, "engine"); return nil }}
	if err := c.store().Put(r); err != nil {
		t.Fatal(err)
	}
	newer := r
	newer.UID = "newer-uid"
	newer.Name = "newer"
	newer.RetentionEligible = false
	newerAt := at.AddDate(0, 0, 1)
	newer.CompletedAt = &newerAt
	newer.Members = []Member{{PVC: "data", UID: "pvc-uid", VolumeName: "pv-data", VolumeHandle: "volume-data", Snapshot: "newer-snapshot", SnapshotUID: "newer-snapshot-uid", Content: "newer-content", ContentUID: "newer-content-uid", SnapshotHandle: "newer-handle", Ready: true}}
	if err := c.store().Put(newer); err != nil {
		t.Fatal(err)
	}
	return c, f, r
}

func (f *pruneKube) run(_ context.Context, input []byte, args ...string) ([]byte, error) {
	if len(args) > 2 && strings.HasPrefix(args[1], "--as=") {
		if args[1] != "--as=system:serviceaccount:example:recovery-workload" {
			f.t.Fatal("unexpected prune identity")
		}
		args = append([]string{args[0]}, args[3:]...)
	}
	if args[0] == "get" {
		switch args[1] {
		case "namespace":
			if f.namespaceGone {
				return nil, nil
			}
			return []byte(`{"metadata":{"uid":"ns-uid"}}`), nil
		case "persistentvolumeclaims":
			if f.consumer {
				return []byte(`{"items":[{"spec":{"dataSource":{"apiGroup":"snapshot.storage.k8s.io","kind":"VolumeSnapshot","name":"snapshot-data"}}}]}`), nil
			}
			return []byte(`{"items":[]}`), nil
		case "volumesnapshot":
			if !f.snapshot {
				return nil, nil
			}
			meta := map[string]any{"uid": "snapshot-uid", "resourceVersion": "1", "labels": map[string]string{Group + "/request-uid": "capture-uid"}}
			if f.deletingSnapshot {
				meta["deletionTimestamp"] = "2026-11-09T00:01:00Z"
			}
			return raw(map[string]any{"metadata": meta, "spec": map[string]any{"volumeSnapshotClassName": SnapshotClass, "source": map[string]string{"persistentVolumeClaimName": "data"}}, "status": map[string]string{"boundVolumeSnapshotContentName": "content-data"}}), nil
		case "volumesnapshotcontent":
			if args[2] == "newer-content" {
				if f.newerGone {
					return nil, nil
				}
				return []byte(`{"metadata":{"uid":"newer-content-uid","resourceVersion":"1"},"spec":{"driver":"btrfs.csi.local","deletionPolicy":"Retain","volumeSnapshotRef":{"uid":"newer-snapshot-uid","name":"newer-snapshot","namespace":"example"},"source":{"volumeHandle":"volume-data"}},"status":{"snapshotHandle":"newer-handle","readyToUse":true}}`), nil
			}
			if !f.content {
				return nil, nil
			}
			return raw(map[string]any{"metadata": map[string]string{"uid": f.contentUID, "resourceVersion": f.rv}, "spec": map[string]any{"driver": "btrfs.csi.local", "deletionPolicy": f.policy, "volumeSnapshotRef": map[string]string{"uid": "snapshot-uid", "name": "snapshot-data", "namespace": "example"}, "source": map[string]string{"volumeHandle": "volume-data"}}, "status": map[string]any{"snapshotHandle": "handle-data", "readyToUse": true}}), nil
		}
	}
	if args[0] == "patch" {
		if args[1] != "volumesnapshotcontent" {
			f.actions = append(f.actions, "status")
			return nil, nil
		}
		var patch []struct {
			Op, Path string
			Value    any
		}
		if err := json.Unmarshal([]byte(args[len(args)-1]), &patch); err != nil {
			return nil, err
		}
		tests := map[string]any{}
		for _, op := range patch {
			if op.Op == "test" {
				tests[op.Path] = op.Value
			}
		}
		for path, want := range map[string]string{"/metadata/uid": f.contentUID, "/metadata/resourceVersion": f.rv, "/spec/source/volumeHandle": "volume-data", "/spec/volumeSnapshotRef/uid": "snapshot-uid", "/status/snapshotHandle": "handle-data", "/spec/deletionPolicy": "Retain"} {
			if tests[path] != want {
				f.t.Fatalf("missing/mismatched mutation precondition %s: %v", path, tests)
			}
		}
		if f.failPatch {
			f.failPatch = false
			return nil, errors.New("injected patch transport failure")
		}
		f.actions = append(f.actions, "arm-delete")
		f.policy = "Delete"
		f.rv = "2"
		return nil, nil
	}
	if args[0] == "delete" && strings.HasPrefix(args[1], "--raw=") {
		var options struct {
			Preconditions struct {
				UID string `json:"uid"`
				RV  string `json:"resourceVersion"`
			} `json:"preconditions"`
		}
		if err := json.Unmarshal(input, &options); err != nil {
			return nil, err
		}
		if strings.Contains(args[1], "/volumesnapshotcontents/") {
			if f.raceContent {
				f.raceContent = false
				f.contentUID = "replacement-uid"
			}
			if options.Preconditions.UID != f.contentUID || options.Preconditions.RV != f.rv {
				return nil, errors.New("DELETE precondition conflict")
			}
			if f.policy != "Delete" {
				f.t.Fatal("content deleted without CSI Delete policy")
			}
			f.actions = append(f.actions, "delete-content")
			f.content = false
		} else {
			if options.Preconditions.UID != "snapshot-uid" || options.Preconditions.RV != "1" {
				f.t.Fatal("snapshot delete lacks exact preconditions")
			}
			if f.policy != "Retain" {
				f.t.Fatal("content deletion armed before snapshot/protection finalizers removed")
			}
			f.actions = append(f.actions, "delete-snapshot")
			if f.pendingSnapshot {
				f.deletingSnapshot = true
			} else {
				f.snapshot = false
			}
		}
		return []byte(`{}`), nil
	}
	return nil, errors.New("unexpected prune command: " + strings.Join(args, " "))
}

func TestPruneJournalResumesAndTombstonePreventsReplay(t *testing.T) {
	c, f, r := pruneFixture(t)
	f.failPatch = true
	if err := c.pruneRecord(context.Background(), &r); err != nil {
		t.Fatal(err)
	} // VS delete
	if r.Phase != "Pruning" || f.snapshot || f.policy != "Retain" {
		t.Fatalf("unsafe first step: %+v %+v", r, f)
	}
	if err := c.pruneRecord(context.Background(), &r); err == nil {
		t.Fatal("expected injected failure after durable arm")
	}
	loaded, err := c.store().Get(r.UID)
	if err != nil {
		t.Fatal(err)
	}
	r = loaded
	if !r.Prune.BackendDone || !r.Prune.Members["content-data"].DeleteArmed {
		t.Fatal("preparation not durable")
	}
	if err := c.pruneRecord(context.Background(), &r); err != nil {
		t.Fatal(err)
	} // retry arm + content delete
	loaded, err = c.store().Get(r.UID)
	if err != nil {
		t.Fatal(err)
	}
	r = loaded
	if err := c.pruneRecord(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Phase != "Pruned" || f.content {
		t.Fatalf("not physically retired: %+v", r)
	}
	joined := strings.Join(f.actions, ",")
	if joined != "engine,delete-snapshot,arm-delete,delete-content" {
		t.Fatalf("unsafe/repeated deletion sequence: %s", joined)
	}
	if err := c.pruneRecord(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	s := specFixture()
	o := Object{Kind: "RecoveryPoint", Metadata: Metadata{Name: "point", Namespace: "example", UID: r.UID, ResourceVersion: "rewound"}, Spec: raw(PointSpec{Capture: &s})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.actions, ",") != joined+",status" {
		t.Fatal("rewound retired UID caused deletion or capture replay")
	}
	if statusFor(o, r).Conditions[0].Status != "False" {
		t.Fatal("pruned point is Ready")
	}
}

func TestPrunePreservesCSIProtectionFinalizers(t *testing.T) {
	c, f, r := pruneFixture(t)
	f.pendingSnapshot = true
	for i := 0; i < 3; i++ {
		if err := c.pruneRecord(context.Background(), &r); err != nil {
			t.Fatal(err)
		}
	}
	if f.policy != "Retain" || !f.content || r.Phase != "Pruning" {
		t.Fatal("snapshot finalizer bypassed")
	}
	if strings.Contains(strings.Join(f.actions, ","), "arm-delete") {
		t.Fatal("armed deletion while snapshot is protected")
	}
}

func TestPruneOrphanedNamespaceAndIdentityConflicts(t *testing.T) {
	t.Run("namespace-deleted", func(t *testing.T) {
		c, f, r := pruneFixture(t)
		f.namespaceGone = true
		for i := 0; i < 3; i++ {
			if err := c.pruneRecord(context.Background(), &r); err != nil {
				t.Fatal(err)
			}
		}
		if r.Phase != "Pruned" || f.content || strings.Contains(strings.Join(f.actions, ","), "delete-snapshot") {
			t.Fatal("orphan pruning did not isolate retained content")
		}
	})
	t.Run("replacement-before-plan", func(t *testing.T) {
		c, f, r := pruneFixture(t)
		f.contentUID = "different-uid"
		if err := c.pruneRecord(context.Background(), &r); err == nil || len(f.actions) != 0 || !f.content {
			t.Fatal("replaced content was pruned")
		}
	})
	t.Run("replacement-at-delete", func(t *testing.T) {
		c, f, r := pruneFixture(t)
		f.namespaceGone = true
		f.raceContent = true
		if err := c.pruneRecord(context.Background(), &r); err == nil || !f.content {
			t.Fatal("DELETE lacked resource preconditions")
		}
		if err := c.pruneRecord(context.Background(), &r); err == nil || !f.content {
			t.Fatal("retry followed new content identity")
		}
	})
}

func TestPruneProtectsDependenciesAndPartialSets(t *testing.T) {
	for _, kind := range []string{"export", "restore", "consumer", "partial"} {
		t.Run(kind, func(t *testing.T) {
			c, f, r := pruneFixture(t)
			switch kind {
			case "export":
				r.ExportPending = true
			case "restore":
				r.ProtectedBy = []string{"restore-uid"}
			case "consumer":
				f.consumer = true
			case "partial":
				r.Phase = "Failed"
			}
			if err := c.pruneRecord(context.Background(), &r); err == nil || len(f.actions) != 0 || !f.content {
				t.Fatal("protected/incomplete set was pruned")
			}
		})
	}
}

func TestPruneResumeCannotDeleteLastSetAfterRewind(t *testing.T) {
	c, f, r := pruneFixture(t)
	if err := c.pruneRecord(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	f.newerGone = true
	if err := c.pruneRecord(context.Background(), &r); err == nil {
		t.Fatal("retirement ignored loss of newer live set")
	}
	if !f.content || f.policy != "Retain" || strings.Contains(strings.Join(f.actions, ","), "arm-delete") {
		t.Fatal("last surviving retained content was armed/deleted")
	}
	f.newerGone = false
	if err := c.pruneRecord(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyKeepLastActuallyPrunesEnginePoints(t *testing.T) {
	c := &Controller{Dir: t.TempDir(), Run: func(context.Context, []byte, ...string) ([]byte, error) { return nil, nil }}
	var pruned []string
	c.Prune = func(_ context.Context, r Record) error { pruned = append(pruned, r.UID); return nil }
	for i, uid := range []string{"old", "latest", "failed"} {
		at := instant("2026-11-09T00:00:00Z").AddDate(0, 0, i)
		r := Record{UID: uid, Kind: "ClusterRecoveryPoint", Scope: "full", PolicyUID: "policy-uid", Phase: "Succeeded", CompletedAt: &at, RecoveryPointID: "point-" + uid}
		if uid == "failed" {
			r.Phase = "Failed"
		}
		if err := c.store().Put(r); err != nil {
			t.Fatal(err)
		}
	}
	o := Object{Kind: "ClusterProtectionPolicy", Metadata: Metadata{Name: "policy", UID: "policy-uid", ResourceVersion: "1"}, Spec: raw(PolicySpec{Scope: "full", Schedule: "0 3 * * *", Suspend: true, KeepLast: 1})}
	if err := c.reconcilePolicy(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	old, _ := c.store().Get("old")
	latest, _ := c.store().Get("latest")
	if old.Phase != "Pruned" || latest.Phase != "Succeeded" || strings.Join(pruned, ",") != "old" {
		t.Fatalf("retention not applied: %v %v %v", old.Phase, latest.Phase, pruned)
	}
}

func TestClusterScopeAndImportedDirectSourceContract(t *testing.T) {
	b := &fakeBackend{}
	f := &fakeKube{t: t}
	c := &Controller{Dir: t.TempDir(), Run: f.run, Backend: b}
	for _, scope := range []string{"system", "full"} {
		o := Object{Kind: "ClusterRecoveryPoint", Metadata: Metadata{Name: "capture-" + scope, UID: "capture-" + scope, ResourceVersion: "1"}, Spec: raw(PointSpec{Scope: scope})}
		if err := c.reconcileRequest(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		if b.ops[len(b.ops)-1].Scope != scope {
			t.Fatal("capture lost explicit scope")
		}
		o = Object{Kind: "ClusterRestoreRequest", Metadata: Metadata{Name: "restore-" + scope, UID: "restore-" + scope, ResourceVersion: "1"}, Spec: raw(RestoreSpec{Scope: scope, DurablePointID: "imported-" + scope})}
		if err := c.reconcileRequest(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		op := b.ops[len(b.ops)-1]
		if op.Scope != scope || op.RecoveryPointID != "imported-"+scope || op.Action != "Restore" {
			t.Fatalf("wrong imported source contract: %+v", op)
		}
	}
	before := b.submits
	for _, scope := range []string{"", "workload"} {
		o := Object{Kind: "ClusterRecoveryPoint", Metadata: Metadata{Name: "invalid", UID: "invalid-" + nonempty(scope, "empty"), ResourceVersion: "1"}, Spec: raw(PointSpec{Scope: scope})}
		if err := c.reconcileRequest(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		r, _ := c.store().Get(o.Metadata.UID)
		if r.Phase != "Failed" || r.Reason != "InvalidScope" {
			t.Fatal("missing/invalid cluster scope accepted")
		}
	}
	s := specFixture()
	o := Object{Kind: "RecoveryPoint", Metadata: Metadata{Name: "invalid", Namespace: "example", UID: "namespace-scope", ResourceVersion: "1"}, Spec: raw(PointSpec{Scope: "full", Capture: &s})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r, _ := c.store().Get(o.Metadata.UID)
	if r.Reason != "InvalidScope" || b.submits != before {
		t.Fatal("namespaced request gained cluster scope")
	}
	if err := (PolicySpec{Schedule: "0 3 * * *", KeepLast: 1}).Validate(true); err == nil {
		t.Fatal("cluster policy omitted scope")
	}
}
