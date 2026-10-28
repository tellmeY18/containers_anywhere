package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeFS struct{ subs map[string]bool }

func (f *fakeFS) CreateSubvolume(_ context.Context, p string) error {
	f.subs[p] = true
	return os.MkdirAll(p, 0o755)
}
func (f *fakeFS) Snapshot(_ context.Context, src, dst string, _ bool) error {
	if !f.subs[src] {
		return os.ErrNotExist
	}
	f.subs[dst] = true
	return copyTree(src, dst)
}
func (f *fakeFS) DeleteSubvolume(_ context.Context, p string) error {
	delete(f.subs, p)
	return os.RemoveAll(p)
}
func (f *fakeFS) IsSubvolume(_ context.Context, p string) bool { return f.subs[p] }

func TestEnsureLayoutAndRestorePointOnVersionChange(t *testing.T) {
	root := t.TempDir()
	varLib, snaps, pool := filepath.Join(root, "lib"), filepath.Join(root, "snaps"), filepath.Join(root, "pool")
	os.MkdirAll(varLib, 0o755)
	os.MkdirAll(snaps, 0o755)
	fs := &fakeFS{subs: map[string]bool{varLib: true}}
	if err := EnsureLayout(context.Background(), fs, varLib); err != nil || len(fs.subs) != 3 {
		t.Fatalf("the image cache and the journal are subvolumes: %v %v", err, fs.subs)
	}
	cfg := RestorePointConfig{VarLib: varLib, SnapshotsDir: snaps, PoolDir: pool, Version: "0.5.0", Keep: 2,
		Now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }}
	if p, err := TakeRestorePointOnChange(context.Background(), fs, cfg); p != "" || err != nil {
		t.Fatalf("a first boot must only record the version: %q %v", p, err)
	}
	if p, _ := TakeRestorePointOnChange(context.Background(), fs, cfg); p != "" {
		t.Fatal("the same version must not take a restore point")
	}
	vol := filepath.Join(pool, "volumes", "vol-1")
	os.MkdirAll(vol, 0o755)
	fs.subs[vol] = true
	os.WriteFile(filepath.Join(pool, "state.json"), []byte(`{"volumes":1}`), 0o600)
	for i, v := range []string{"0.5.1", "0.5.2", "0.5.3"} {
		cfg.Version = v
		cfg.Now = func() time.Time { return time.Date(2026, 10, 28, 13+i, 0, 0, 0, time.UTC) }
		if p, err := TakeRestorePointOnChange(context.Background(), fs, cfg); err != nil || p == "" {
			t.Fatalf("%s: %q %v", v, p, err)
		}
	}
	entries, _ := os.ReadDir(snaps)
	if len(entries) != 2 {
		t.Fatalf("only the newest 2 restore points are kept: %v", entries)
	}
	newest := filepath.Join(snaps, entries[1].Name())
	if _, err := os.Stat(filepath.Join(newest, "vol-vol-1")); err != nil {
		t.Fatal("pool volumes must be part of the restore point")
	}
	if b, err := os.ReadFile(filepath.Join(newest, "pool-state.json")); err != nil || string(b) != `{"volumes":1}` {
		t.Fatal("the driver's state file must be kept with the restore point")
	}
}

func TestSessionMarkerDetectsAnUncleanEndAndTheImageCacheIsCleared(t *testing.T) {
	varLib := t.TempDir()
	if unclean, err := BeginSession(varLib); unclean || err != nil {
		t.Fatalf("a first boot is not unclean: %v %v", unclean, err)
	}
	cache := filepath.Join(varLib, imageCache)
	os.MkdirAll(filepath.Join(cache, "io.containerd.snapshotter.v1.overlayfs", "snapshots", "1"), 0o755)
	os.WriteFile(filepath.Join(cache, "containerd.log"), []byte("x"), 0o600)
	vol := filepath.Join(varLib, "btrfs-csi-pool/volumes/v1")
	os.MkdirAll(vol, 0o755)
	os.WriteFile(filepath.Join(vol, "keep"), []byte("data"), 0o600)
	if unclean, _ := BeginSession(varLib); !unclean {
		t.Fatal("a session that never ended must be reported unclean")
	}
	if err := ResetImageCache(varLib); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Fatalf("image cache not emptied: %v", entries)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatal("the cache directory itself must remain")
	}
	if _, err := os.Stat(filepath.Join(vol, "keep")); err != nil {
		t.Fatal("workload volumes must never be touched")
	}
	if err := EndSession(varLib); err != nil {
		t.Fatal(err)
	}
	if unclean, _ := BeginSession(varLib); unclean {
		t.Fatal("a clean shutdown must not be reported as unclean")
	}
	if err := ResetImageCache(t.TempDir()); err != nil {
		t.Fatal("a missing cache is not an error:", err)
	}
}

// copyTree copies the files under src into dst (created if needed). Tests use it for snapshots
// and as the restore's copier.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
