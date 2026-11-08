package recoveryapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func raw(v any) []byte { b, _ := json.Marshal(v); return b }
func instant(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
func specFixture() CaptureSpec {
	return CaptureSpec{Selector: Selector{MatchLabels: map[string]string{"set": "app"}}, ExpectedPVCs: []string{"data", "uploads"}, Consistency: "PerPVC", TimeoutSeconds: 10}
}
func pvcFixture(name, uid string, labels map[string]string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "uid": uid, "labels": labels}, "spec": map[string]any{"volumeName": "pv-" + name, "storageClassName": "btrfs", "accessModes": []string{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]string{"storage": "10Gi"}}}, "status": map[string]string{"phase": "Bound"}}
}

func TestExactSelectorMembershipAndUIDs(t *testing.T) {
	s := specFixture()
	a, b := pvcFixture("data", "u1", map[string]string{"set": "app"}), pvcFixture("uploads", "u2", map[string]string{"set": "app"})
	members, err := SelectPVCs(s, raw(map[string]any{"items": []any{b, a}}))
	if err != nil || len(members) != 2 || members[0].UID != "u1" {
		t.Fatalf("selection: %+v, %v", members, err)
	}
	for name, items := range map[string][]any{
		"missing": {a}, "unexpected": {a, b, pvcFixture("other", "u3", map[string]string{"set": "app"})},
		"duplicate": {a, a}, "missing-label": {a, pvcFixture("uploads", "u2", nil)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SelectPVCs(s, raw(map[string]any{"items": items})); err == nil {
				t.Fatal("accepted inexact membership")
			}
		})
	}
	changed := append([]Member(nil), members...)
	changed[0].UID = "replacement"
	if sameMembers(members, changed) {
		t.Fatal("same-name replacement accepted")
	}
	s.Selector.MatchLabels = map[string]string{"absent": ""}
	if _, err := SelectPVCs(s, raw(map[string]any{"items": []any{a, b}})); err == nil {
		t.Fatal("missing label matched empty string")
	}
}

func TestScheduleBoundsUTCAndClockReplay(t *testing.T) {
	for _, expr := range []string{"* * * * *", "*/10 * * * *", "0 0 0 * *", "0 25 * * *", "0 0 * 13 *", "0 0 * * 8", "0 0 * * MON", "@daily", "0/0 * * * *", "0,,30 * * * *", "+0 * * * *", "0 0 * * * *"} {
		if _, err := ParseSchedule(expr); err == nil {
			t.Errorf("accepted %q", expr)
		}
	}
	s, err := ParseSchedule("0,15,30,45 0-23/2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	now := instant("2026-11-09T12:52:11Z")
	due, ok := s.Due(now, now.Add(-72*time.Hour))
	if !ok || !due.Equal(instant("2026-11-09T12:45:00Z")) {
		t.Fatalf("due %s %v", due, ok)
	}
	if _, ok := s.Due(now, due); ok {
		t.Fatal("same slot replayed")
	}
	if _, ok := s.Due(now.Add(-time.Hour), due); ok {
		t.Fatal("backward clock replayed")
	}
	if !s.Matches(instant("2026-11-09T18:00:00+05:30")) {
		t.Fatal("cron not evaluated in UTC")
	}
	daily, _ := ParseSchedule("0 3 * * *")
	if _, ok := daily.Due(instant("2026-11-09T02:59:00Z"), instant("2026-11-09T00:00:00Z")); ok {
		t.Fatal("ran before first schedule")
	}
	leap, _ := ParseSchedule("0 0 29 2 *")
	if !leap.Matches(instant("2028-02-29T00:00:00Z")) || leap.Matches(instant("2026-02-28T00:00:00Z")) {
		t.Fatal("invalid leap-day semantics")
	}
	or, _ := ParseSchedule("0 0 1 * 1")
	if !or.Matches(instant("2026-11-09T00:00:00Z")) || !or.Matches(instant("2026-12-01T00:00:00Z")) {
		t.Fatal("restricted DOM/DOW must use OR")
	}
}

func TestRetentionPreservesLastSuccessFailuresExportsAndRestores(t *testing.T) {
	base := instant("2026-11-09T00:00:00Z")
	var records []Record
	for i, uid := range []string{"old", "export", "restore", "latest", "failed"} {
		at := base.Add(time.Duration(i) * time.Hour)
		r := Record{UID: uid, Kind: "RecoveryPoint", PolicyUID: "policy", Phase: "Succeeded", CompletedAt: &at}
		if uid == "export" {
			r.ExportPending = true
		}
		if uid == "restore" {
			r.ProtectedBy = []string{"restore-request"}
		}
		if uid == "failed" {
			r.Phase = "Failed"
		}
		records = append(records, r)
	}
	if got := RetentionCandidates(records, "policy", 1); !reflect.DeepEqual(got, []string{"old"}) {
		t.Fatalf("unsafe candidates: %v", got)
	}
	if got := RetentionCandidates(records, "policy", 0); len(got) != 0 {
		t.Fatal("zero keep deletes data")
	}
	if got := RetentionCandidates(records[len(records)-2:], "policy", 1); len(got) != 0 {
		t.Fatal("failure displaced last successful set")
	}
}

func TestFileStoreRejectsCorruptAndTraversingCatalog(t *testing.T) {
	s := FileStore{Dir: t.TempDir()}
	r := Record{UID: "uid-1", Kind: "RecoveryPoint", Phase: "Prepared"}
	if err := s.Put(r); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(r.UID); err != nil || got.Phase != r.Phase {
		t.Fatalf("read: %+v %v", got, err)
	}
	if err := s.Put(Record{UID: "../escape"}); err == nil {
		t.Fatal("accepted path traversal")
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "uid-1.json"), []byte(`{"uid":"uid-1"} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("corrupt journal was ignored")
	}
}

type fakeKube struct {
	t           *testing.T
	snapshots   map[string]map[string]any
	failPVC     string
	checkFails  bool
	commands    []string
	destination DestinationSpec
}

func (f *fakeKube) run(_ context.Context, input []byte, args ...string) ([]byte, error) {
	full := strings.Join(args, " ")
	f.commands = append(f.commands, full)
	impersonated := len(args) > 2 && strings.HasPrefix(args[1], "--as=system:serviceaccount:example:recovery-workload")
	if impersonated {
		args = append([]string{args[0]}, args[3:]...)
	}
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	verb := args[0]
	if verb == "patch" {
		return []byte(`{}`), nil
	}
	if verb == "exec" {
		if !impersonated {
			f.t.Fatal("hook ran as admin")
		}
		if strings.HasSuffix(full, "check") && f.checkFails {
			return nil, errors.New("still quiesced")
		}
		return nil, nil
	}
	if verb == "create" {
		if !impersonated {
			f.t.Fatal("workload create ran as admin")
		}
		var obj map[string]any
		if err := json.Unmarshal(input, &obj); err != nil {
			return nil, err
		}
		meta := obj["metadata"].(map[string]any)
		name := meta["name"].(string)
		if obj["kind"] == "VolumeSnapshot" {
			source := obj["spec"].(map[string]any)["source"].(map[string]any)["persistentVolumeClaimName"].(string)
			if source == f.failPVC {
				return nil, errors.New("injected second-member CSI failure")
			}
			meta["uid"] = "snapshot-" + name
			meta["resourceVersion"] = "1"
			obj["status"] = map[string]any{"readyToUse": true, "boundVolumeSnapshotContentName": "content-" + name}
			f.snapshots[name] = obj
		}
		return raw(obj), nil
	}
	if verb != "get" || len(args) < 2 {
		return nil, errors.New("unexpected fake command: " + full)
	}
	resource := args[1]
	switch resource {
	case "namespace":
		return []byte(`{"metadata":{"uid":"namespace-uid"}}`), nil
	case "volumesnapshotclass":
		return []byte(`{"driver":"btrfs.csi.local","deletionPolicy":"Retain"}`), nil
	case "persistentvolumeclaims":
		if !impersonated {
			f.t.Fatal("PVC discovery ran as admin")
		}
		return raw(map[string]any{"items": []any{pvcFixture("data", "u1", map[string]string{"set": "app"}), pvcFixture("uploads", "u2", map[string]string{"set": "app"})}}), nil
	case "persistentvolume":
		name := strings.TrimPrefix(args[2], "pv-")
		uid := "u1"
		if name == "uploads" {
			uid = "u2"
		}
		return raw(map[string]any{"spec": map[string]any{"claimRef": map[string]string{"uid": uid, "name": name, "namespace": "example"}, "csi": map[string]string{"driver": "btrfs.csi.local", "volumeHandle": "volume-" + name}}}), nil
	case "pod":
		if !impersonated {
			f.t.Fatal("hook pod lookup ran as admin")
		}
		return []byte(`{"metadata":{"uid":"pod-uid"},"spec":{"containers":[{"name":"application"}]}}`), nil
	case "volumesnapshots.snapshot.storage.k8s.io", "volumesnapshot":
		if !impersonated {
			f.t.Fatal("snapshot lookup ran as admin")
		}
		if obj, ok := f.snapshots[args[2]]; ok {
			return raw(obj), nil
		}
		return nil, nil
	case "volumesnapshotcontent":
		name := strings.TrimPrefix(args[2], "content-")
		obj := f.snapshots[name]
		source := obj["spec"].(map[string]any)["source"].(map[string]any)["persistentVolumeClaimName"].(string)
		return raw(map[string]any{"metadata": map[string]string{"uid": "content-uid-" + name, "resourceVersion": "1"}, "spec": map[string]any{"driver": "btrfs.csi.local", "deletionPolicy": "Retain", "volumeSnapshotRef": map[string]string{"uid": "snapshot-" + name, "name": name, "namespace": "example"}, "source": map[string]string{"volumeHandle": "volume-" + source}}, "status": map[string]any{"readyToUse": true, "snapshotHandle": "handle-" + name}}), nil
	case "backupdestinations." + Group:
		return raw(map[string]any{"metadata": map[string]string{"uid": "destination-uid"}, "spec": f.destination}), nil
	}
	return nil, errors.New("unexpected fake resource: " + full)
}

func quiescedFixture() CaptureSpec {
	s := specFixture()
	s.Consistency = "QuiescedGroup"
	s.Hooks = []Hook{{Pod: "example-0", Container: "application", Prepare: []string{"prepare"}, Resume: []string{"resume"}, Check: []string{"check"}, TimeoutSeconds: 1}}
	return s
}

func TestPartialCaptureAlwaysChecksResumeAndRetainsJournal(t *testing.T) {
	f := &fakeKube{t: t, snapshots: map[string]map[string]any{}, failPVC: "uploads"}
	c := &Controller{Dir: t.TempDir(), Run: f.run}
	s := quiescedFixture()
	o := Object{Kind: "RecoveryPoint", Metadata: Metadata{Name: "point", Namespace: "example", UID: "point-uid", ResourceVersion: "1"}, Spec: raw(PointSpec{Capture: &s})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r, err := c.store().Get(o.Metadata.UID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != "Failed" || r.Reason != "CaptureFailed" || !r.Hooks[0].Resumed || !r.Members[0].Ready || r.Members[1].Ready {
		t.Fatalf("unsafe partial state: %+v", r)
	}
	if len(f.snapshots) != 1 || r.Members[0].SnapshotHandle == "" {
		t.Fatal("partial snapshot identity lost")
	}
	joined := strings.Join(f.commands, "\n")
	if !strings.Contains(joined, "-- resume") || !strings.Contains(joined, "-- check") || strings.Contains(joined, "delete") {
		t.Fatalf("unsafe cleanup: %s", joined)
	}
	count := len(f.snapshots)
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(f.snapshots) != count {
		t.Fatal("failed UID replayed capture")
	}
}

func TestResumeFailureIsDurableAndRestartRetriesOnlyResume(t *testing.T) {
	f := &fakeKube{t: t, snapshots: map[string]map[string]any{}, failPVC: "uploads", checkFails: true}
	c := &Controller{Dir: t.TempDir(), Run: f.run}
	s := quiescedFixture()
	o := Object{Kind: "RecoveryPoint", Metadata: Metadata{Name: "point", Namespace: "example", UID: "point-uid", ResourceVersion: "1"}, Spec: raw(PointSpec{Capture: &s})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r, err := c.store().Get(o.Metadata.UID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != "ResumeFailed" || r.Hooks[0].Resumed {
		t.Fatalf("failed check hidden: %+v", r)
	}
	f.checkFails = false
	f.commands = nil
	if err := c.recoverInterrupted(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Phase != "Failed" || !r.Hooks[0].Resumed {
		t.Fatal("interrupted capture reported success")
	}
	for _, cmd := range f.commands {
		if strings.Contains(cmd, "-- prepare") || strings.Contains(cmd, "create") {
			t.Fatalf("unsafe replay: %s", cmd)
		}
	}
}

type fakeBackend struct {
	submits, gets int
	ops           []ClusterOperation
}

func (b *fakeBackend) Submit(_ context.Context, op ClusterOperation) (OperationStatus, error) {
	b.submits++
	b.ops = append(b.ops, op)
	id := op.RecoveryPointID
	if op.Action == "Capture" {
		id = "engine-point"
	}
	return OperationStatus{Phase: "Succeeded", RecoveryPointID: id}, nil
}
func (b *fakeBackend) Get(context.Context, string) (OperationStatus, error) {
	b.gets++
	return OperationStatus{}, errors.New("unexpected replay")
}

func TestRestoredRequestUIDCannotReplayClusterRestore(t *testing.T) {
	b := &fakeBackend{}
	c := &Controller{Dir: t.TempDir(), Backend: b, Run: func(context.Context, []byte, ...string) ([]byte, error) { return []byte(`{}`), nil }}
	point := Record{UID: "source-uid", Kind: "ClusterRecoveryPoint", Name: "source", Scope: "full", Phase: "Succeeded", RecoveryPointID: "engine-point"}
	if err := c.store().Put(point); err != nil {
		t.Fatal(err)
	}
	o := Object{Kind: "ClusterRestoreRequest", Metadata: Metadata{Name: "restore", UID: "restore-uid", ResourceVersion: "1"}, Spec: raw(RestoreSpec{Scope: "full", RecoveryPoint: "source", RecoveryPointUID: "source-uid"})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// Disk rewind resurrects the same Kubernetes request UID with empty status.
	o.Metadata.ResourceVersion = "999"
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if b.submits != 1 || b.gets != 0 {
		t.Fatalf("restore replayed: %+v", b)
	}
	protected, err := c.store().Get(point.UID)
	if err != nil || !reflect.DeepEqual(protected.ProtectedBy, []string{"restore-uid"}) {
		t.Fatalf("restore protection missing: %+v %v", protected, err)
	}
	o.Spec = raw(RestoreSpec{Scope: "full", RecoveryPoint: "different", RecoveryPointUID: "source-uid"})
	if err := c.reconcileRequest(context.Background(), o); err == nil {
		t.Fatal("same UID accepted altered spec")
	}
}

func TestDestinationsAreTruthfulAndNamespaceAuthorized(t *testing.T) {
	f := &fakeKube{t: t, destination: DestinationSpec{Provider: "HostDirectory", HostDirectoryID: "enrolled", AllowedNamespaces: []string{"other"}}}
	c := &Controller{Dir: t.TempDir(), Run: f.run, HostDestination: func(context.Context, string) (DestinationStatus, error) {
		return DestinationStatus{Conditions: []Condition{{Type: "Ready", Status: "True", Reason: "Enrolled"}}}, nil
	}}
	for _, provider := range []string{"S3Compatible", "AzureBlob", "GCS", "SFTP", "RepositoryService"} {
		st, err := c.Destination(context.Background(), DestinationSpec{Provider: provider})
		if err != nil || len(st.Conditions) != 1 || st.Conditions[0].Status != "False" || st.Conditions[0].Reason != "NotImplemented" {
			t.Fatalf("provider %s overstated: %+v %v", provider, st, err)
		}
	}
	if _, err := c.destination(context.Background(), "dest", "example"); err == nil {
		t.Fatal("unauthorized namespace accepted")
	}
	st, _ := c.Destination(context.Background(), DestinationSpec{Provider: "HostDirectory", HostDirectoryID: "/host/path"})
	if st.Conditions[0].Reason != "InvalidSpec" {
		t.Fatal("host path accepted")
	}
	f.destination.AllowedNamespaces = []string{"example"}
	calls := 0
	c.Export = func(context.Context, Record, Destination) (ExportStatus, error) {
		calls++
		if calls == 1 {
			return ExportStatus{}, errors.New("offline")
		}
		return ExportStatus{Phase: "Succeeded", Reason: "VerifiedExport", Receipt: "opaque-receipt"}, nil
	}
	r := Record{UID: "point-uid", Kind: "RecoveryPoint", Phase: "Succeeded", Namespace: "example", NamespaceUID: "namespace-uid", Destination: "dest", DestinationUID: "destination-uid", ExportPending: true}
	if err := c.exportPoint(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if !r.ExportPending || r.Export.Phase != "Failed" {
		t.Fatal("failed export released retention")
	}
	if err := c.exportPoint(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if r.ExportPending || r.Export.Phase != "Succeeded" {
		t.Fatal("successful export not recorded")
	}
	if err := c.exportPoint(context.Background(), &r); err != nil || calls != 2 {
		t.Fatal("completed export replayed")
	}
	f.destination.AllowedNamespaces = nil
	r.ExportPending = true
	if err := c.exportPoint(context.Background(), &r); err != nil || calls != 2 || !r.ExportPending {
		t.Fatal("revoked namespace exported")
	}
}

func TestImportedRestoreUIDReplayAndScopedContract(t *testing.T) {
	f := &fakeKube{t: t}
	c := &Controller{Dir: t.TempDir(), Run: f.run}
	calls := 0
	c.RestoreImported = func(_ context.Context, op ImportedRestoreRequest) (ImportedRestoreStatus, error) {
		calls++
		if op.Namespace != "example" || op.NamespaceUID != "namespace-uid" || op.ServiceAccount != WorkloadServiceAccount || op.PointID != "imported-point" || op.RequestUID != "import-request" {
			t.Fatalf("unscoped import: %+v", op)
		}
		if _, err := c.store().Get(op.RequestUID); err != nil {
			t.Fatalf("import started before durable journal: %v", err)
		}
		return ImportedRestoreStatus{Phase: "Succeeded", Reason: "PopulatedFreshPVCs", RestoredPVCs: op.PVCNames}, nil
	}
	o := Object{Kind: "RestoreRequest", Metadata: Metadata{Name: "import", Namespace: "example", UID: "import-request", ResourceVersion: "1"}, Spec: raw(RestoreSpec{DurablePointID: "imported-point", PVCNames: map[string]string{"data": "new-data"}})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("imported restore UID replayed")
	}
	o.Metadata.UID = "ambiguous-request"
	o.Spec = raw(RestoreSpec{DurablePointID: "imported-point", RecoveryPoint: "local", RecoveryPointUID: "local-uid", PVCNames: map[string]string{"data": "new-data"}})
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r, err := c.store().Get(o.Metadata.UID)
	if err != nil || r.Phase != "Failed" || calls != 1 {
		t.Fatal("ambiguous import source reached adapter")
	}
}

func TestRewoundCSIInventoryDoesNotReplayOrClaimLocalAvailability(t *testing.T) {
	f := &fakeKube{t: t, snapshots: map[string]map[string]any{}}
	c := &Controller{Dir: t.TempDir(), Run: f.run}
	s := specFixture()
	o := Object{Kind: "RecoveryPoint", Metadata: Metadata{Name: "point", Namespace: "example", UID: "point-uid", ResourceVersion: "1"}, Spec: raw(PointSpec{Capture: &s})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r, err := c.store().Get(o.Metadata.UID)
	if err != nil || r.Phase != "Succeeded" {
		t.Fatalf("capture: %+v %v", r, err)
	}
	f.snapshots = map[string]map[string]any{} // cluster/data inventory was rewound
	f.commands = nil
	if err := c.localAvailable(context.Background(), r); err == nil {
		t.Fatal("host catalog falsely implies live CSI handles")
	}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.commands, "\n")
	if strings.Contains(joined, "create") || !strings.Contains(joined, "LocalSnapshotUnavailable") {
		t.Fatalf("unsafe replay/status: %s", joined)
	}
	request := Object{Kind: "RestoreRequest", Metadata: Metadata{Name: "restore", Namespace: "example", UID: "restore-uid", ResourceVersion: "1"}, Spec: raw(RestoreSpec{RecoveryPoint: "point", RecoveryPointUID: "point-uid", PVCNames: map[string]string{"data": "restored-data", "uploads": "restored-uploads"}})}
	// Simulate the real API's NotFound error rather than an empty body.
	c.Run = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		if args[0] == "get" && strings.Contains(strings.Join(args, " "), "volumesnapshot ") {
			return nil, errors.New("NotFound")
		}
		return f.run(ctx, input, args...)
	}
	if err := c.reconcileRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	restored, err := c.store().Get(request.Metadata.UID)
	if err != nil || restored.Reason != "SourceUnavailable" || restored.Phase != "Failed" {
		t.Fatalf("rewound source not refused: %+v %v", restored, err)
	}
}

func TestCancelledCaptureUsesDetachedCheckedResume(t *testing.T) {
	f := &fakeKube{t: t, snapshots: map[string]map[string]any{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resumed, checked := false, false
	c := &Controller{Dir: t.TempDir()}
	c.Run = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		command := strings.Join(args, " ")
		if args[0] == "create" {
			cancel()
			return nil, ctx.Err()
		}
		if strings.Contains(command, "-- resume") {
			resumed = true
			if ctx.Err() != nil {
				t.Fatal("resume inherited cancelled context")
			}
		}
		if strings.Contains(command, "-- check") {
			checked = true
			if ctx.Err() != nil {
				t.Fatal("resume check inherited cancelled context")
			}
		}
		return f.run(ctx, input, args...)
	}
	s := quiescedFixture()
	o := Object{Kind: "RecoveryPoint", Metadata: Metadata{Name: "point", Namespace: "example", UID: "point-uid", ResourceVersion: "1"}, Spec: raw(PointSpec{Capture: &s})}
	if err := c.reconcileRequest(ctx, o); err != nil {
		t.Fatal(err)
	}
	if !resumed || !checked {
		t.Fatal("cancelled capture did not resume and verify")
	}
}
