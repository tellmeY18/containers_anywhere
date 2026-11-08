package recoveryapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOfflineDestinationBoundsManyScheduledSlotsAndCoalescesRecovery(t *testing.T) {
	now := instant("2026-11-09T00:00:00Z")
	base := now
	creates, exports := 0, 0
	enrolled := false
	c := &Controller{Dir: t.TempDir(), Now: func() time.Time { return now }}
	c.HostDestination = func(context.Context, string) (DestinationStatus, error) {
		status := "False"
		if enrolled {
			status = "True"
		}
		return DestinationStatus{Conditions: []Condition{{Type: "Ready", Status: status, Reason: "EnrollmentStatus"}}}, nil
	}
	c.Export = func(context.Context, Record, Destination) (ExportStatus, error) {
		exports++
		return ExportStatus{Phase: "Succeeded", Receipt: "verified-export", Reason: "VerifiedExport"}, nil
	}
	c.Prune = func(context.Context, Record) error { return nil }
	c.Run = func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		switch args[0] {
		case "patch":
			return []byte(`{}`), nil
		case "get":
			if args[1] == "backupdestinations."+Group {
				return raw(map[string]any{"metadata": map[string]string{"uid": "destination-uid"}, "spec": DestinationSpec{Provider: "HostDirectory", HostDirectoryID: "enrolled-drive"}}), nil
			}
			return nil, nil
		case "create":
			var obj Object
			if err := json.Unmarshal(input, &obj); err != nil {
				return nil, err
			}
			var spec PointSpec
			if err := json.Unmarshal(obj.Spec, &spec); err != nil {
				return nil, err
			}
			creates++
			at := now
			// Simulate completed cold captures while their destination is offline.
			r := Record{UID: "uid-" + obj.Metadata.Name, Name: obj.Metadata.Name, Kind: "ClusterRecoveryPoint", Scope: spec.Scope, PolicyUID: spec.PolicyUID, Phase: "Succeeded", CompletedAt: &at, RecoveryPointID: "engine-" + obj.Metadata.Name, ExportPending: true, Destination: spec.Destination, DestinationUID: "destination-uid", BackendAttempted: true, BackendSubmitted: true}
			return nil, c.store().Put(r)
		}
		return nil, errors.New("unexpected command")
	}
	o := Object{Kind: "ClusterProtectionPolicy", Metadata: Metadata{Name: "policy", UID: "policy-uid", ResourceVersion: "1", CreationTimestamp: base.Add(-time.Minute)}, Spec: raw(PolicySpec{Scope: "full", Schedule: "*/15 * * * *", KeepLast: 2, Destination: "offline"})}
	for i := 0; i < 32; i++ {
		now = base.Add(time.Duration(i) * 15 * time.Minute)
		if err := c.reconcilePolicy(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		all, err := c.store().List()
		if err != nil {
			t.Fatal(err)
		}
		for _, point := range all {
			if point.ExportPending {
				if err := c.exportPoint(context.Background(), &point); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	policy, _ := c.store().Get(o.Metadata.UID)
	if creates != 2 || exports != 0 || policy.Reason != "CaptureBacklog" || policy.Phase != "Holding" || policy.LastSchedule == nil || !policy.LastSchedule.Equal(base.Add(15*time.Minute)) {
		t.Fatalf("outage backlog unbounded/cursor advanced: captures=%d exports=%d policy=%+v", creates, exports, policy)
	}
	enrolled = true
	all, err := c.store().List()
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range all {
		if point.ExportPending {
			if err := c.exportPoint(context.Background(), &point); err != nil {
				t.Fatal(err)
			}
		}
	}
	now = now.Add(15 * time.Minute)
	if err := c.reconcilePolicy(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	policy, _ = c.store().Get(o.Metadata.UID)
	if creates != 3 || exports != 2 || policy.Phase != "Active" || policy.LastSchedule == nil || !policy.LastSchedule.Equal(now) {
		t.Fatalf("recovery did not coalesce to one latest slot: %d %d %+v", creates, exports, policy)
	}
}

func TestScheduleBudgetAllowsOneReplacementAndBlocksRetainedBacklog(t *testing.T) {
	c := &Controller{Dir: t.TempDir()}
	policy := Record{UID: "policy-uid"}
	put := func(uid, phase string, pending bool) {
		t.Helper()
		if err := c.store().Put(Record{UID: uid, Kind: "ClusterRecoveryPoint", PolicyUID: policy.UID, Phase: phase, ExportPending: pending, BackendSubmitted: true}); err != nil {
			t.Fatal(err)
		}
	}
	put("latest", "Succeeded", false)
	if reason, _, err := c.scheduleGate(context.Background(), &policy, 1); err != nil || reason != "" {
		t.Fatalf("newest-complete preservation deadlocked healthy rotation: %s %v", reason, err)
	}
	put("retiring", "Pruning", false)
	if reason, _, err := c.scheduleGate(context.Background(), &policy, 1); err != nil || reason != "RetentionBlocked" {
		t.Fatalf("retiring content ignored by budget: %s %v", reason, err)
	}
	put("retiring", "Pruned", false)
	put("latest", "Succeeded", true)
	if reason, _, err := c.scheduleGate(context.Background(), &policy, 1); err != nil || reason != "CaptureBacklog" {
		t.Fatalf("pending export ignored: %s %v", reason, err)
	}
	policy.AwaitingCapture = "unobserved-request"
	if reason, _, err := c.scheduleGate(context.Background(), &policy, 100); err != nil || reason != "CaptureBacklog" {
		t.Fatalf("unobserved CR did not reserve a slot: %s %v", reason, err)
	}
}

func TestCapacityFailurePrecedesPrepareAndClusterSubmission(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		t.Run(map[bool]string{false: "workload", true: "cluster"}[cluster], func(t *testing.T) {
			f := &fakeKube{t: t}
			b := &fakeBackend{}
			checks := 0
			c := &Controller{Dir: t.TempDir(), Run: f.run, Backend: b, CheckCapacity: func(context.Context) error { checks++; return errors.New("reserve exhausted") }}
			s := quiescedFixture()
			o := Object{Kind: "RecoveryPoint", Metadata: Metadata{Name: "point", Namespace: "example", UID: "capture-uid", ResourceVersion: "1"}, Spec: raw(PointSpec{Capture: &s})}
			if cluster {
				o.Kind = "ClusterRecoveryPoint"
				o.Metadata.Namespace = ""
				o.Spec = raw(PointSpec{Scope: "full"})
			}
			if err := c.reconcileRequest(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			r, err := c.store().Get(o.Metadata.UID)
			if err != nil {
				t.Fatal(err)
			}
			if r.Phase != "Failed" || r.Reason != "StoragePressure" || checks != 1 || b.submits != 0 || len(r.Hooks) != 0 || len(r.Members) != 0 {
				t.Fatalf("capacity check ran too late: %+v checks=%d submissions=%d", r, checks, b.submits)
			}
			for _, cmd := range f.commands {
				if strings.HasPrefix(cmd, "exec ") || strings.HasPrefix(cmd, "create ") {
					t.Fatalf("storage pressure allowed side effect: %s", cmd)
				}
			}
			if err := c.reconcileRequest(context.Background(), o); err != nil || checks != 1 {
				t.Fatal("status reconciliation invoked capacity check")
			}
		})
	}
}

func TestPolicyStoragePressureHoldsCursorAndRetriesLatestSlot(t *testing.T) {
	now := instant("2026-11-09T00:00:00Z")
	base := now
	pressure := true
	creates := 0
	c := &Controller{Dir: t.TempDir(), Now: func() time.Time { return now }, CheckCapacity: func(context.Context) error {
		if pressure {
			return errors.New("full")
		}
		return nil
	}}
	c.Run = func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		if args[0] == "create" {
			creates++
		}
		return nil, nil
	}
	o := Object{Kind: "ClusterProtectionPolicy", Metadata: Metadata{Name: "policy", UID: "policy-uid", ResourceVersion: "1", CreationTimestamp: base.Add(-time.Minute)}, Spec: raw(PolicySpec{Scope: "system", Schedule: "*/15 * * * *", KeepLast: 2})}
	for i := 0; i < 8; i++ {
		now = base.Add(time.Duration(i) * 15 * time.Minute)
		if err := c.reconcilePolicy(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		policy, _ := c.store().Get(o.Metadata.UID)
		if policy.Reason != "StoragePressure" || policy.LastSchedule != nil || policy.PendingSchedule != nil || creates != 0 {
			t.Fatalf("pressure advanced schedule: %+v", policy)
		}
	}
	pressure = false
	if err := c.reconcilePolicy(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	policy, _ := c.store().Get(o.Metadata.UID)
	if creates != 1 || policy.LastSchedule == nil || !policy.LastSchedule.Equal(now) {
		t.Fatal("pressure recovery did not coalesce latest slot")
	}
}

func TestCapacityCheckDoesNotGateRestoresOrSubmittedOperations(t *testing.T) {
	f := &fakeKube{t: t}
	b := &fakeBackend{}
	checks := 0
	c := &Controller{Dir: t.TempDir(), Run: f.run, Backend: b, CheckCapacity: func(context.Context) error { checks++; return errors.New("full") }, RestoreImported: func(_ context.Context, r ImportedRestoreRequest) (ImportedRestoreStatus, error) {
		return ImportedRestoreStatus{Phase: "Succeeded", RestoredPVCs: r.PVCNames}, nil
	}}
	o := Object{Kind: "RestoreRequest", Metadata: Metadata{Name: "restore", Namespace: "example", UID: "restore-uid", ResourceVersion: "1"}, Spec: raw(RestoreSpec{DurablePointID: "imported", PVCNames: map[string]string{"data": "new-data"}})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o = Object{Kind: "ClusterRestoreRequest", Metadata: Metadata{Name: "restore", UID: "cluster-restore-uid", ResourceVersion: "1"}, Spec: raw(RestoreSpec{Scope: "full", DurablePointID: "imported-cluster"})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r := Record{UID: "running-capture", Kind: "ClusterRecoveryPoint", Scope: "full", Phase: "Running", BackendSubmitted: true}
	_ = c.cluster(context.Background(), &r, ClusterOperation{UID: r.UID, Action: "Capture", Scope: "full"}) // fake Get returns an error, never a new submission
	if checks != 0 || b.submits != 1 || b.gets != 1 {
		t.Fatalf("capacity gate affected restore/status: checks=%d backend=%+v", checks, b)
	}
}
