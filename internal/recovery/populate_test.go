//go:build linux || darwin

package recovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func populationFixture(t *testing.T) (fixture, Manifest, PopulateRequest) {
	t.Helper()
	f := setup(t)
	i, _ := f.fs.sub(filepath.Join(f.e.cfg.PoolDir, "snapshots/s1"))
	f.fs.subs[i].ro = true
	m, err := f.e.RegisterSnapshots(context.Background(), RegisterRequest{ID: "workload-source", Members: []SnapshotMember{{PVCNamespace: "ns", PVCName: "data", PVCUID: "pvcuid", PVName: "pvc-pvcuid", VolumeHandle: "v1", SnapshotHandle: "s1", SnapshotUID: "snapshotuid", SnapshotContentUID: "contentuid", Driver: "btrfs.csi.local"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.fs.CreateSubvolume(context.Background(), filepath.Join(f.e.cfg.PoolDir, "volumes/new-volume")); err != nil {
		t.Fatal(err)
	}
	s, _, err := readDriver(filepath.Join(f.e.cfg.PoolDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Volumes["new-volume"] = &driverVolume{ID: "new-volume", Name: "pvc-new-pvc-uid", CapacityBytes: 4096}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	f.write("btrfs-csi-pool/state.json", string(b))
	req := PopulateRequest{UID: "populate-request", PointID: m.ID, Namespace: "ns", NamespaceUID: "namespace-uid", Targets: []PopulateTarget{{SourceSnapshotHandle: "s1", TargetVolumeHandle: "new-volume", TargetPVCUID: "new-pvc-uid", TargetPVCName: "restored", TargetPVName: "pvc-new-pvc-uid"}}}
	return f, m, req
}

func TestPopulateWorkloadPreservesCSIIdentityQuotaAndSource(t *testing.T) {
	f, m, req := populationFixture(t)
	before := f.read("btrfs-csi-pool/state.json")
	result, err := f.e.PopulateWorkload(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != "completed" || result.Targets[0].Phase != "completed" {
		t.Fatal(result)
	}
	if f.read("btrfs-csi-pool/volumes/new-volume/data") != "snapshot-volume" || f.read("btrfs-csi-pool/volumes/v1/data") != "original-volume" || f.read("btrfs-csi-pool/state.json") != before {
		t.Fatal("wrong contents or driver state changed")
	}
	f.write("btrfs-csi-pool/volumes/new-volume/data", "new application writes")
	f.fs.mounted = true
	// Durable completion must not copy again over a now-used target.
	result, err = f.e.PopulateWorkload(context.Background(), req)
	if err != nil || result.Phase != "completed" {
		t.Fatal(result, err)
	}
	if f.read("btrfs-csi-pool/volumes/new-volume/data") != "new application writes" {
		t.Fatal("idempotent retry overwrote application data")
	}
	if err := f.e.Delete(context.Background(), m.ID); err == nil {
		t.Fatal("restore source deleted")
	}
}

func TestPopulateRefusesMountedAndNonemptyTargets(t *testing.T) {
	t.Run("mounted", func(t *testing.T) {
		f, _, req := populationFixture(t)
		f.fs.mounted = true
		if _, err := f.e.PopulateWorkload(context.Background(), req); err == nil {
			t.Fatal("mounted target populated")
		}
		if entries, _ := os.ReadDir(filepath.Join(f.e.cfg.PoolDir, "volumes/new-volume")); len(entries) != 0 {
			t.Fatal("target changed")
		}
	})
	t.Run("nonempty", func(t *testing.T) {
		f, _, req := populationFixture(t)
		f.write("btrfs-csi-pool/volumes/new-volume/user-data", "must survive")
		if _, err := f.e.PopulateWorkload(context.Background(), req); err == nil {
			t.Fatal("nonempty target populated")
		}
		if f.read("btrfs-csi-pool/volumes/new-volume/user-data") != "must survive" {
			t.Fatal("target overwritten")
		}
	})
}

func TestPopulateInterruptedNonemptyTargetIsRetainedAndRefused(t *testing.T) {
	f, _, req := populationFixture(t)
	original := f.e.cfg.Copy
	f.e.cfg.Copy = func(ctx context.Context, s, d string) error {
		if err := original(ctx, s, d); err != nil {
			return err
		}
		panic("power-loss-before-completion-journal")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fault not injected")
			}
		}()
		_, _ = f.e.PopulateWorkload(context.Background(), req)
	}()
	state, err := f.e.PopulationStatus(req.UID)
	if err != nil || state.Targets[0].Phase != "copying" {
		t.Fatal(state, err)
	}
	f.write("btrfs-csi-pool/volumes/new-volume/data", "possibly used after interruption")
	f.e.cfg.Copy = original
	if _, err := f.e.PopulateWorkload(context.Background(), req); err == nil {
		t.Fatal("ambiguous retry accepted")
	}
	if f.read("btrfs-csi-pool/volumes/new-volume/data") != "possibly used after interruption" {
		t.Fatal("ambiguous target overwritten")
	}
	state, err = f.e.PopulationStatus(req.UID)
	if err != nil || state.Phase != "failed" {
		t.Fatal(state, err)
	}
}
