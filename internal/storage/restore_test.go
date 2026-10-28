package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type restoreFixture struct {
	t                         *testing.T
	root, varLib, snaps, pool string
	fs                        *fakeFS
	cfg                       RestoreConfig
	point                     string
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// newRestoreFixture builds a disk whose current state ("new") differs from a restore point ("old").
func newRestoreFixture(t *testing.T) *restoreFixture {
	root := t.TempDir()
	f := &restoreFixture{t: t, root: root, varLib: filepath.Join(root, "lib"), snaps: filepath.Join(root, "snaps"), pool: filepath.Join(root, "lib", "btrfs-csi-pool")}
	f.fs = &fakeFS{subs: map[string]bool{f.varLib: true, f.pool: true}}
	cache := filepath.Join(f.varLib, "rancher/k3s/agent/containerd")
	journal := filepath.Join(f.varLib, "containers-anywhere/journal")
	f.fs.subs[cache], f.fs.subs[journal] = true, true
	write(t, filepath.Join(f.varLib, "rancher/k3s/server/db/state.db"), "new-cluster-db")
	write(t, filepath.Join(f.varLib, "kubelet/pods/p1/x"), "new-kubelet")
	write(t, filepath.Join(f.varLib, "containers-anywhere/boot.json"), `{"version":"0.5.0"}`)
	write(t, filepath.Join(cache, "blob"), "cached-image")
	write(t, filepath.Join(journal, "boot.journal"), "live-journal")
	vol := filepath.Join(f.pool, "volumes", "vol-1")
	f.fs.subs[vol] = true
	write(t, filepath.Join(vol, "data"), "new-volume-data")
	write(t, filepath.Join(f.pool, "volumes", "vol-extra", "data"), "volume-made-later")
	f.fs.subs[filepath.Join(f.pool, "volumes", "vol-extra")] = true
	write(t, filepath.Join(f.pool, "state.json"), "new-pool-state")

	f.point = "restore-20261028T120000Z-from-0.5.0"
	dir := filepath.Join(f.snaps, f.point)
	f.fs.subs[filepath.Join(dir, "system")] = true
	write(t, filepath.Join(dir, "system/rancher/k3s/server/db/state.db"), "old-cluster-db")
	write(t, filepath.Join(dir, "system/containers-anywhere/boot.json"), `{"version":"0.5.0"}`)
	f.fs.subs[filepath.Join(dir, "vol-vol-1")] = true
	write(t, filepath.Join(dir, "vol-vol-1/data"), "old-volume-data")
	write(t, filepath.Join(dir, "pool-state.json"), "old-pool-state")
	write(t, filepath.Join(dir, "meta.json"), `{"from":"0.5.0","to":"0.5.1","createdAt":"2026-10-28T12:00:00Z","volumes":["vol-1"]}`)

	n := 0
	f.cfg = RestoreConfig{
		RestorePointConfig: RestorePointConfig{VarLib: f.varLib, SnapshotsDir: f.snaps, PoolDir: f.pool, Version: "0.5.0", Keep: 2,
			Now: func() time.Time { n++; return time.Date(2026, 10, 29, 9, 0, n, 0, time.UTC) }},
		Copy: func(_ context.Context, src, dst string) error { return copyTree(src, dst) },
	}
	return f
}

func (f *restoreFixture) request() error {
	return RequestRestore(f.snaps, f.point, f.cfg.Version, time.Now())
}

func TestARestoreReplacesTheStateAndKeepsTheOldOneAsARestorePoint(t *testing.T) {
	f := newRestoreFixture(t)
	if err := f.request(); err != nil {
		t.Fatal(err)
	}
	if PendingRestore(f.snaps) != f.point {
		t.Fatal("the request must be recorded")
	}
	done, err := ApplyPendingRestore(context.Background(), f.fs, f.cfg)
	if !done || err != nil {
		t.Fatalf("%v %v", done, err)
	}
	if got := read(filepath.Join(f.varLib, "rancher/k3s/server/db/state.db")); got != "old-cluster-db" {
		t.Fatalf("the cluster database was not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.varLib, "kubelet")); err == nil {
		t.Fatal("state that is not in the restore point must be gone")
	}
	if got := read(filepath.Join(f.pool, "volumes", "vol-1", "data")); got != "old-volume-data" {
		t.Fatalf("the volume was not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.pool, "volumes", "vol-extra")); err == nil {
		t.Fatal("a volume made after the restore point must not survive in the pool")
	}
	if got := read(filepath.Join(f.pool, "state.json")); got != "old-pool-state" {
		t.Fatalf("the pool's claim map must come from the point: %q", got)
	}
	if got := read(filepath.Join(f.varLib, "containers-anywhere/journal/boot.journal")); got != "live-journal" {
		t.Fatal("the live journal must be left alone")
	}
	if _, err := os.Stat(filepath.Join(f.varLib, "rancher/k3s/agent/containerd/blob")); err == nil {
		t.Fatal("the image cache must be emptied")
	}
	if PendingRestore(f.snaps) != "" {
		t.Fatal("the request must be cleared")
	}
	res, ok := LastRestore(f.snaps)
	if !ok || !res.OK || res.Safety == "" || res.RolledBack {
		t.Fatalf("%+v", res)
	}
	// The state that was replaced is still there, and so is the point restored from.
	safety := filepath.Join(f.snaps, res.Safety)
	if got := read(filepath.Join(safety, "system/rancher/k3s/server/db/state.db")); got != "new-cluster-db" {
		t.Fatalf("the replaced state must be kept: %q", got)
	}
	if got := read(filepath.Join(safety, "vol-vol-extra/data")); got != "volume-made-later" {
		t.Fatalf("the replaced volumes must be kept: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.snaps, f.point)); err != nil {
		t.Fatal("the restore point itself must survive pruning")
	}
}

func TestRestoreRefusesWhatItCannotDo(t *testing.T) {
	f := newRestoreFixture(t)
	if err := RequestRestore(f.snaps, "../etc", "0.5.0", time.Now()); err == nil {
		t.Fatal("a path is not a restore point")
	}
	if err := RequestRestore(f.snaps, "restore-20200101T000000Z-from-9.9", "0.5.0", time.Now()); err == nil {
		t.Fatal("an unknown point must be refused")
	}
	if err := RequestRestore(f.snaps, f.point, "0.6.0", time.Now()); err == nil || !strings.Contains(err.Error(), "matching appliance") {
		t.Fatalf("a point from another appliance version must be refused with the reason: %v", err)
	}
	if err := f.request(); err != nil {
		t.Fatal(err)
	}
	if err := f.request(); err == nil {
		t.Fatal("a second request while one is waiting must be refused")
	}
	// The appliance changed between the request and the boot: nothing is touched.
	f.cfg.Version = "0.6.0"
	done, err := ApplyPendingRestore(context.Background(), f.fs, f.cfg)
	if done || err == nil {
		t.Fatal("a version mismatch at boot must refuse")
	}
	if read(filepath.Join(f.varLib, "rancher/k3s/server/db/state.db")) != "new-cluster-db" {
		t.Fatal("nothing may change on a refusal")
	}
	if PendingRestore(f.snaps) != "" {
		t.Fatal("a refused request must not loop at every boot")
	}
}

func TestAFailedRestorePutsThePreviousStateBack(t *testing.T) {
	f := newRestoreFixture(t)
	f.request()
	calls := 0
	f.cfg.Copy = func(ctx context.Context, src, dst string) error {
		calls++
		if calls == 1 {
			_ = copyTree(src, dst) // part of it lands, then it fails
			return errors.New("disk error")
		}
		return copyTree(src, dst)
	}
	done, err := ApplyPendingRestore(context.Background(), f.fs, f.cfg)
	if done || err == nil || !strings.Contains(err.Error(), "put back") {
		t.Fatalf("%v %v", done, err)
	}
	if got := read(filepath.Join(f.varLib, "rancher/k3s/server/db/state.db")); got != "new-cluster-db" {
		t.Fatalf("the previous state must be back: %q", got)
	}
	if got := read(filepath.Join(f.pool, "volumes", "vol-extra", "data")); got != "volume-made-later" {
		t.Fatalf("the previous volumes must be back: %q", got)
	}
	if res, _ := LastRestore(f.snaps); !res.RolledBack || res.OK {
		t.Fatalf("%+v", res)
	}
	if PendingRestore(f.snaps) != "" {
		t.Fatal("a rolled-back restore must not be retried at every boot")
	}
}

func TestAnInterruptedRestoreResumesWithoutTakingASecondSafetyPoint(t *testing.T) {
	f := newRestoreFixture(t)
	f.request()
	f.cfg.Copy = func(context.Context, string, string) error { return errors.New("power cut") }
	if done, err := ApplyPendingRestore(context.Background(), f.fs, f.cfg); done || err == nil || !strings.Contains(err.Error(), "will not start") {
		t.Fatalf("when nothing can be put back the cluster must not start: %v %v", done, err)
	}
	if PendingRestore(f.snaps) != f.point {
		t.Fatal("the restore must stay pending so the next boot resumes it")
	}
	before := countPoints(f.snaps)
	f.cfg.Copy = func(_ context.Context, src, dst string) error { return copyTree(src, dst) }
	done, err := ApplyPendingRestore(context.Background(), f.fs, f.cfg)
	if !done || err != nil {
		t.Fatalf("%v %v", done, err)
	}
	if after := countPoints(f.snaps); after != before {
		t.Fatalf("resuming must not take another safety point: %d -> %d", before, after)
	}
	if read(filepath.Join(f.varLib, "rancher/k3s/server/db/state.db")) != "old-cluster-db" {
		t.Fatal("the resumed restore must complete")
	}
}

func TestRestorePointsAreListedWithWhetherTheyCanBeRestored(t *testing.T) {
	f := newRestoreFixture(t)
	list, err := ListRestorePoints(f.snaps, "0.5.0")
	if err != nil || len(list) != 1 || !list[0].Restorable || list[0].From != "0.5.0" || len(list[0].Volumes) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	list, _ = ListRestorePoints(f.snaps, "0.6.0")
	if list[0].Restorable || list[0].Reason == "" {
		t.Fatalf("%+v", list)
	}
}

func countPoints(dir string) int {
	n := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && pointName.MatchString(e.Name()) {
			n++
		}
	}
	return n
}
