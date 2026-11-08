package recoveryapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMissingHostJournalNeverReplaysHistoricalRequests(t *testing.T) {
	for _, kind := range []string{"RecoveryPoint", "RestoreRequest", "ClusterRecoveryPoint", "ClusterRestoreRequest"} {
		for _, phase := range []string{"Prepared", "ResumeFailed", "Succeeded", "Failed"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				b := &fakeBackend{}
				f := &fakeKube{t: t}
				calls := 0
				c := &Controller{Dir: t.TempDir(), Run: func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
					if args[0] != "patch" && !(args[0] == "get" && args[1] == "namespace") {
						t.Fatalf("historical request executed command: %v", args)
					}
					return f.run(ctx, input, args...)
				}, Backend: b, Export: func(context.Context, Record, Destination) (ExportStatus, error) { calls++; return ExportStatus{}, nil }, RestoreImported: func(context.Context, ImportedRestoreRequest) (ImportedRestoreStatus, error) {
					calls++
					return ImportedRestoreStatus{}, nil
				}}
				s := quiescedFixture()
				namespace := "example"
				spec := raw(PointSpec{Capture: &s})
				if strings.HasPrefix(kind, "Cluster") {
					namespace = ""
					spec = raw(PointSpec{Scope: "full"})
				}
				if kind == "RestoreRequest" {
					spec = raw(RestoreSpec{DurablePointID: "old-point", PVCNames: map[string]string{"data": "new-data"}})
				}
				if kind == "ClusterRestoreRequest" {
					spec = raw(RestoreSpec{Scope: "full", DurablePointID: "old-point"})
				}
				o := Object{Kind: kind, Metadata: Metadata{Name: "historical", Namespace: namespace, UID: "historical-uid", Generation: 1, ResourceVersion: "1"}, Spec: spec, Status: &Status{ObservedGeneration: 1, Phase: phase}}
				if err := c.reconcileRequest(context.Background(), o); err != nil {
					t.Fatal(err)
				}
				r, err := c.store().Get(o.Metadata.UID)
				if err != nil {
					t.Fatal(err)
				}
				if r.Phase != "Failed" || r.Reason != "HostJournalMissing" || len(r.ProtectedBy) == 0 || b.submits != 0 || calls != 0 {
					t.Fatalf("historical replay was not quarantined: %+v", r)
				}
				gets := b.gets
				if err := c.reconcileRequest(context.Background(), o); err != nil {
					t.Fatal(err)
				}
				if b.submits != 0 || b.gets != gets || calls != 0 {
					t.Fatal("quarantined UID replayed")
				}
				if kind == "RecoveryPoint" && phase != "Succeeded" {
					if !r.ResumeUncertain {
						t.Fatal("unknown hook journal did not flag resume uncertainty")
					}
					o.Metadata.UID = "new-uid"
					o.Status = nil
					if err := c.reconcileRequest(context.Background(), o); err != nil {
						t.Fatal(err)
					}
					blocked, _ := c.store().Get(o.Metadata.UID)
					if blocked.Reason != "ResumeBlocked" {
						t.Fatalf("new prepare ran across unresolved historic hooks: %+v", blocked)
					}
				}
			})
		}
	}
}

type historicalBackend struct{ gets, submits int }

func (b *historicalBackend) Submit(context.Context, ClusterOperation) (OperationStatus, error) {
	b.submits++
	return OperationStatus{}, errors.New("historical submit forbidden")
}
func (b *historicalBackend) Get(context.Context, string) (OperationStatus, error) {
	b.gets++
	return OperationStatus{Phase: "Succeeded", RecoveryPointID: "guest-durable-point"}, nil
}

func TestHistoricalClusterTerminalResultUsesGetOnlyAndStaysProtected(t *testing.T) {
	b := &historicalBackend{}
	c := &Controller{Dir: t.TempDir(), Backend: b, Run: func(context.Context, []byte, ...string) ([]byte, error) { return []byte(`{}`), nil }}
	o := Object{Kind: "ClusterRecoveryPoint", Metadata: Metadata{Name: "old", UID: "old-uid", ResourceVersion: "1"}, Spec: raw(PointSpec{Scope: "system"}), Status: &Status{Phase: "Running", ObservedGeneration: 1}}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r, _ := c.store().Get(o.Metadata.UID)
	if r.Phase != "Succeeded" || r.Reason != "GuestJournalRecovered" || r.Scope != "system" || r.RecoveryPointID != "guest-durable-point" || len(r.ProtectedBy) == 0 || b.gets != 1 || b.submits != 0 {
		t.Fatalf("unsafe historical adoption: %+v", r)
	}
	if err := c.reconcileRequest(context.Background(), o); err != nil || b.gets != 1 || b.submits != 0 {
		t.Fatal("recovered result replayed")
	}
}

func TestStartedStatusPrecedesSideEffectsAndAdvancesCASVersion(t *testing.T) {
	f := &fakeKube{t: t, snapshots: map[string]map[string]any{}}
	started, finished := false, false
	c := &Controller{Dir: t.TempDir()}
	c.Run = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		if args[0] == "patch" {
			var encoded string
			for i, arg := range args {
				if arg == "-p" {
					encoded = args[i+1]
				}
			}
			var patch []struct {
				Op, Path string
				Value    json.RawMessage
			}
			if err := json.Unmarshal([]byte(encoded), &patch); err != nil {
				t.Fatal(err)
			}
			var status Status
			var rv string
			for _, op := range patch {
				if op.Path == "/status" {
					_ = json.Unmarshal(op.Value, &status)
				}
				if op.Path == "/metadata/resourceVersion" {
					_ = json.Unmarshal(op.Value, &rv)
				}
			}
			if status.Phase == "Prepared" {
				started = true
				return []byte(`{"metadata":{"uid":"capture-uid","resourceVersion":"2"}}`), nil
			}
			if status.Phase == "Succeeded" {
				if !started || rv != "2" {
					t.Fatalf("final status lost CAS: started=%v rv=%s", started, rv)
				}
				finished = true
			}
		}
		if (args[0] == "create" || args[0] == "exec") && !started {
			t.Fatal("side effect preceded durable Kubernetes start marker")
		}
		return f.run(ctx, input, args...)
	}
	s := quiescedFixture()
	o := Object{Kind: "RecoveryPoint", Metadata: Metadata{Name: "point", Namespace: "example", UID: "capture-uid", ResourceVersion: "1"}, Spec: raw(PointSpec{Capture: &s})}
	if err := c.reconcileRequest(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !started || !finished {
		t.Fatal("capture status transition incomplete")
	}
}
