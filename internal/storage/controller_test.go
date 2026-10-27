package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeFS struct {
	subs  map[string]bool
	nocow []string
	free  float64
}

func (f *fakeFS) CreateSubvolume(_ context.Context, p string) error {
	f.subs[p] = true
	return os.MkdirAll(p, 0o755)
}
func (f *fakeFS) Snapshot(_ context.Context, src, dst string, _ bool) error {
	if !f.subs[src] {
		return os.ErrNotExist
	}
	f.subs[dst] = true
	return os.MkdirAll(dst, 0o755)
}
func (f *fakeFS) DeleteSubvolume(_ context.Context, p string) error {
	delete(f.subs, p)
	return os.RemoveAll(p)
}
func (f *fakeFS) IsSubvolume(_ context.Context, p string) bool { return f.subs[p] }
func (f *fakeFS) SetNoCOW(_ context.Context, p string) error {
	f.nocow = append(f.nocow, p)
	return nil
}
func (f *fakeFS) FreeFraction(string) (float64, error) { return f.free, nil }

type fakeKube struct {
	lists    map[string][]Object
	applied  []Object
	deleted  []string
	statuses map[string]Object
}

func (k *fakeKube) List(_ context.Context, r string) ([]Object, error) { return k.lists[r], nil }
func (k *fakeKube) Apply(_ context.Context, o Object) error {
	k.applied = append(k.applied, o)
	return nil
}
func (k *fakeKube) Delete(_ context.Context, r, ns, n string) error {
	k.deleted = append(k.deleted, r+"/"+ns+"/"+n)
	return nil
}
func (k *fakeKube) SetStatus(_ context.Context, _, ns, n string, st Object) error {
	k.statuses[ns+"/"+n] = st
	return nil
}

func pvc(ns, name, uid, class, phase string) Object {
	return Object{"metadata": Object{"namespace": ns, "name": name, "uid": uid},
		"spec":   Object{"storageClassName": class, "resources": Object{"requests": Object{"storage": "2Gi"}}},
		"status": Object{"phase": phase}}
}

func newFixture(t *testing.T) (*Controller, *fakeKube, *fakeFS) {
	root := t.TempDir()
	fs := &fakeFS{subs: map[string]bool{}, free: 0.5}
	k := &fakeKube{lists: map[string][]Object{}, statuses: map[string]Object{}}
	k.lists["storageclass"] = []Object{
		{"metadata": Object{"name": "std"}, "provisioner": Provisioner},
		{"metadata": Object{"name": "raw"}, "provisioner": Provisioner, "parameters": Object{"cow": "false"}},
		{"metadata": Object{"name": "other"}, "provisioner": "example.com/x"},
	}
	k.lists[snapshotRes], k.lists[scheduleRes] = nil, nil
	c := NewController(k, fs, filepath.Join(root, "vol"), filepath.Join(root, "snap"))
	c.Now = func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }
	os.MkdirAll(c.VolumesDir, 0o755)
	os.MkdirAll(c.SnapshotsDir, 0o755)
	return c, k, fs
}

func TestProvisionsOneSubvolumePerClaimAndHonoursClass(t *testing.T) {
	c, k, fs := newFixture(t)
	k.lists["pvc"] = []Object{pvc("app", "data", "11111111-aaaa", "std", "Pending"), pvc("app", "db", "22222222-bbbb", "raw", "Pending"),
		pvc("app", "x", "33333333-cccc", "other", "Pending"), pvc("app", "bound", "44444444-dddd", "std", "Bound")}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(k.applied) != 2 || len(fs.nocow) != 1 || !strings.Contains(fs.nocow[0], "/app/db-") {
		t.Fatalf("applied=%d nocow=%v", len(k.applied), fs.nocow)
	}
	spec := k.applied[0]["spec"].(Object)
	if spec["persistentVolumeReclaimPolicy"] != "Retain" || spec["claimRef"].(Object)["uid"] != "11111111-aaaa" || spec["capacity"].(Object)["storage"] != "2Gi" {
		t.Fatalf("%v", spec)
	}
	if !within(c.VolumesDir, spec["hostPath"].(Object)["path"].(string)) {
		t.Fatal("volume outside the volumes directory")
	}
}

func TestRemovesVolumeOfDeletedClaimButNeverABoundOne(t *testing.T) {
	c, k, fs := newFixture(t)
	gone := c.volumePath("app", "old", "55555555-eeee")
	os.MkdirAll(gone, 0o755)
	fs.subs[gone] = true
	bound := c.volumePath("app", "live", "66666666-ffff")
	os.MkdirAll(bound, 0o755)
	fs.subs[bound] = true
	pv := func(uid, path, phase string) Object {
		return Object{"metadata": Object{"name": "pvc-" + uid, "labels": Object{labelManaged: "true"}, "annotations": Object{annPath: path}},
			"spec": Object{"claimRef": Object{"uid": uid}}, "status": Object{"phase": phase}}
	}
	k.lists["pv"] = []Object{pv("55555555-eeee", gone, "Released"), pv("66666666-ffff", bound, "Bound"),
		pv("77777777-0000", "/etc", "Released")}
	k.lists["pvc"] = []Object{pvc("app", "live", "66666666-ffff", "std", "Bound")}
	err := c.Reconcile(context.Background())
	if fs.subs[gone] || !fs.subs[bound] || len(k.deleted) != 1 {
		t.Fatalf("gone=%v bound=%v deleted=%v", fs.subs[gone], fs.subs[bound], k.deleted)
	}
	if err == nil || !strings.Contains(err.Error(), "outside the volumes directory") {
		t.Fatalf("a path outside the volumes directory must be refused: %v", err)
	}
}

func TestSnapshotCloneCapsAndReserve(t *testing.T) {
	c, k, fs := newFixture(t)
	c.MaxPerNamespace = 1
	src := c.volumePath("app", "data", "11111111-aaaa")
	os.MkdirAll(src, 0o755)
	fs.subs[src] = true
	k.lists["pvc"] = []Object{func() Object {
		p := pvc("app", "data", "11111111-aaaa", "std", "Bound")
		p["spec"].(Object)["volumeName"] = "pvc-11111111-aaaa"
		return p
	}()}
	k.lists["pv"] = []Object{{"metadata": Object{"name": "pvc-11111111-aaaa", "labels": Object{labelManaged: "true"}, "annotations": Object{annPath: src}},
		"spec": Object{"claimRef": Object{"uid": "11111111-aaaa"}}, "status": Object{"phase": "Bound"}}}
	snap := func(name, uid, created string) Object {
		return Object{"metadata": Object{"namespace": "app", "name": name, "uid": uid, "creationTimestamp": created},
			"spec": Object{"persistentVolumeClaimName": "data"}}
	}
	k.lists[snapshotRes] = []Object{snap("a", "aaaaaaaa-1", "2026-10-28T10:00:00Z"), snap("b", "bbbbbbbb-2", "2026-10-28T10:01:00Z")}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.statuses["app/a"]["phase"] != "Ready" || k.statuses["app/b"]["phase"] != "Failed" || !fs.subs[c.snapshotPath("aaaaaaaa-1")] {
		t.Fatalf("%v", k.statuses)
	}
	// A ready snapshot can be cloned; the clone is a separate subvolume.
	ready := snap("a", "aaaaaaaa-1", "2026-10-28T10:00:00Z")
	ready["status"] = Object{"phase": "Ready"}
	k.lists[snapshotRes] = []Object{ready}
	clone := pvc("app", "copy", "88888888-1111", "std", "Pending")
	clone["spec"].(Object)["dataSourceRef"] = Object{"apiGroup": Group, "kind": SnapshotKind, "name": "a"}
	k.lists["pvc"] = append(k.lists["pvc"], clone)
	k.applied = nil
	if err := c.Reconcile(context.Background()); err != nil || len(k.applied) != 1 || !fs.subs[c.volumePath("app", "copy", "88888888-1111")] {
		t.Fatalf("clone: %v %d", err, len(k.applied))
	}
	// Below the free-space reserve nothing new is cloned or snapshotted.
	fs.free = 0.05
	k.lists["pvc"] = []Object{pvc("app", "copy2", "99999999-2222", "std", "Pending")}
	k.lists["pvc"][0]["spec"].(Object)["dataSourceRef"] = Object{"apiGroup": Group, "kind": SnapshotKind, "name": "a"}
	if err := c.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("expected a free space refusal: %v", err)
	}
	// An orphaned snapshot subvolume is collected; a live one is kept.
	orphan := c.snapshotPath("deadbeef-9")
	os.MkdirAll(orphan, 0o755)
	fs.subs[orphan] = true
	fs.free = 0.5
	k.lists["pvc"] = nil
	c.Reconcile(context.Background())
	if fs.subs[orphan] || !fs.subs[c.snapshotPath("aaaaaaaa-1")] {
		t.Fatal("garbage collection is wrong")
	}
}

func TestScheduleCreatesThenPrunes(t *testing.T) {
	c, k, _ := newFixture(t)
	sched := Object{"metadata": Object{"namespace": "app", "name": "hourly", "uid": "s1"},
		"spec": Object{"persistentVolumeClaimName": "data", "intervalMinutes": float64(60), "keep": float64(1)}}
	k.lists[scheduleRes] = []Object{sched}
	c.Reconcile(context.Background())
	if len(k.applied) != 1 || k.applied[0]["metadata"].(Object)["name"] != "hourly-20261028-1200" {
		t.Fatalf("%v", k.applied)
	}
	child := func(name, created, phase string) Object {
		return Object{"metadata": Object{"namespace": "app", "name": name, "uid": "u-" + name, "creationTimestamp": created, "labels": Object{labelSchedule: "hourly"}},
			"status": Object{"phase": phase}}
	}
	k.applied = nil
	k.lists[snapshotRes] = []Object{child("old", "2026-10-28T09:00:00Z", "Ready"), child("new", "2026-10-28T11:30:00Z", "Ready")}
	c.Reconcile(context.Background())
	if len(k.applied) != 0 || len(k.deleted) != 1 || !strings.HasSuffix(k.deleted[0], "/old") {
		t.Fatalf("applied=%v deleted=%v", k.applied, k.deleted)
	}
}

func TestEnsureLayoutAndRestorePointOnVersionChange(t *testing.T) {
	root := t.TempDir()
	varLib, snaps := filepath.Join(root, "lib"), filepath.Join(root, "snaps")
	os.MkdirAll(varLib, 0o755)
	os.MkdirAll(snaps, 0o755)
	fs := &fakeFS{subs: map[string]bool{varLib: true}}
	if err := EnsureLayout(context.Background(), fs, varLib); err != nil || len(fs.subs) != 4 {
		t.Fatalf("%v %v", err, fs.subs)
	}
	cfg := RestorePointConfig{VarLib: varLib, SnapshotsDir: snaps, VolumesDir: filepath.Join(varLib, "rancher/k3s/storage"), Version: "0.4.0", Keep: 2,
		Now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }}
	if p, err := TakeRestorePointOnChange(context.Background(), fs, cfg); p != "" || err != nil {
		t.Fatalf("a first boot must only record the version: %q %v", p, err)
	}
	if p, _ := TakeRestorePointOnChange(context.Background(), fs, cfg); p != "" {
		t.Fatal("the same version must not take a restore point")
	}
	vol := filepath.Join(cfg.VolumesDir, "app", "data-1")
	os.MkdirAll(vol, 0o755)
	fs.subs[vol] = true
	for i, v := range []string{"0.4.1", "0.4.2", "0.4.3"} {
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
	if _, err := os.Stat(filepath.Join(snaps, entries[1].Name(), "vol-app-data-1")); err != nil {
		t.Fatal("volumes must be part of the restore point")
	}
}
