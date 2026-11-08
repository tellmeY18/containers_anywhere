//go:build linux || darwin

package recovery

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/storage"
)

type quotaRestoreBackend struct {
	Backend
	pool                   string
	mode                   string
	prepared, volumeClones int
	limits                 map[string]int64
}

func (b *quotaRestoreBackend) PreparePool(_ context.Context, path string) error {
	if path != filepath.Dir(b.pool) {
		return errors.New("quota preparation must use the existing filesystem, not a missing fresh pool")
	}
	if b.mode == "" {
		b.mode = "squota"
	}
	if b.mode != "squota" {
		return errors.New("incompatible quota mode")
	}
	b.prepared++
	return nil
}

func (b *quotaRestoreBackend) Snapshot(ctx context.Context, src, dst string, readOnly bool) error {
	if filepath.Dir(dst) == filepath.Join(b.pool, "volumes") {
		if b.mode != "squota" || b.prepared == 0 {
			return errors.New("restored volume cloned before simple quotas were initialized")
		}
		b.volumeClones++
	}
	return b.Backend.Snapshot(ctx, src, dst, readOnly)
}

func (b *quotaRestoreBackend) SetLimit(ctx context.Context, path string, size int64) error {
	if b.mode != "squota" {
		return errors.New("quota not enabled")
	}
	if !b.Backend.IsSubvolume(ctx, path) {
		return errors.New("quota limit target is not a restored subvolume")
	}
	b.limits[path] = size
	return nil
}

func TestImportedFullRestoreBootstrapsFreshPoolSimpleQuotas(t *testing.T) {
	ctx := context.Background()
	source := setup(t)
	m := source.capture("portable-full", "full")
	var archive bytes.Buffer
	if err := source.e.Export(ctx, m.ID, &archive); err != nil {
		t.Fatal(err)
	}
	fresh := setup(t)
	// Like a fresh healthy VM with hostPath workloads: CSI's pool can exist,
	// but no PVC has ever caused the driver to enable filesystem quotas.
	if err := storage.RemoveTree(ctx, fresh.fs, fresh.e.cfg.PoolDir); err != nil {
		t.Fatal(err)
	}
	if err := fresh.fs.CreateSubvolume(ctx, fresh.e.cfg.PoolDir); err != nil {
		t.Fatal(err)
	}
	fresh.write("btrfs-csi-pool/state.json", `{"volumes":{},"snapshots":{}}`)
	fresh.write("database", "fresh-database")
	fresh.write("mapping", "no-pvs")
	backend := &quotaRestoreBackend{Backend: fresh.fs, pool: fresh.e.cfg.PoolDir, limits: map[string]int64{}}
	fresh.e.cfg.FS = backend
	if _, err := fresh.e.Import(ctx, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	if backend.mode != "" {
		t.Fatal("import unexpectedly activated CSI quota/pool state")
	}
	for i, uid := range []string{"fresh-restore", "repeated-restore"} {
		if _, err := fresh.e.Submit(ctx, Request{ID: uid, Action: "restore", Scope: "full", PointID: m.ID}); err != nil {
			t.Fatal(err)
		}
		if err := fresh.e.ProcessCold(ctx); err != nil {
			t.Fatal(err)
		}
		op, err := fresh.e.Status(uid)
		if err != nil || op.Phase != "completed" {
			t.Fatalf("full restore failed: %+v %v", op, err)
		}
		if backend.mode != "squota" || backend.prepared != i+1 || backend.volumeClones != i+1 || backend.limits[filepath.Join(fresh.e.cfg.PoolDir, "volumes/v1")] != 4096 {
			t.Fatalf("restored quotas missing or enabled too late: %+v", backend)
		}
		if fresh.read("btrfs-csi-pool/volumes/v1/data") != "original-volume" || fresh.read("btrfs-csi-pool/snapshots/s1/data") != "snapshot-volume" || fresh.read("database") != "original-db" {
			t.Fatal("full restore did not recover its datastore, volumes and CSI snapshots")
		}
	}
}
