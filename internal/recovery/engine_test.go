//go:build linux || darwin

package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Inode-backed fake subvolumes survive directory renames like real Btrfs. A
// parent snapshot excludes nested subvolume contents (the important property
// that ordinary recursive-copy mocks get wrong).
type fakeSub struct {
	info os.FileInfo
	ro   bool
}
type fakeBackend struct {
	subs         []fakeSub
	failSnapshot string
	received     int
	mounted      bool
}

func (f *fakeBackend) sub(path string) (int, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	for i, s := range f.subs {
		if os.SameFile(st, s.info) {
			return i, true
		}
	}
	return 0, false
}
func (f *fakeBackend) CreateSubvolume(_ context.Context, path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err == nil {
		f.subs = append(f.subs, fakeSub{info: st})
	}
	return err
}
func (f *fakeBackend) IsSubvolume(_ context.Context, path string) bool {
	_, ok := f.sub(path)
	return ok
}
func (f *fakeBackend) Snapshot(ctx context.Context, src, dst string, ro bool) error {
	if filepath.Base(dst) == f.failSnapshot {
		return errors.New("injected snapshot failure")
	}
	if !f.IsSubvolume(ctx, src) {
		return errors.New("not a subvolume")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		return errors.New("destination exists")
	}
	if err := f.CreateSubvolume(ctx, dst); err != nil {
		return err
	}
	if err := f.copy(src, dst, true); err != nil {
		return err
	}
	i, _ := f.sub(dst)
	f.subs[i].ro = ro
	return nil
}
func (f *fakeBackend) copy(src, dst string, skipSubs bool) error {
	if err := os.MkdirAll(dst, 0700); err != nil {
		return err
	}
	es, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, ent := range es {
		s, d := filepath.Join(src, ent.Name()), filepath.Join(dst, ent.Name())
		if ent.IsDir() {
			if err := os.MkdirAll(d, 0700); err != nil {
				return err
			}
			if _, ok := f.sub(s); skipSubs && ok {
				continue
			}
			if err := f.copy(s, d, skipSubs); err != nil {
				return err
			}
		} else {
			b, err := os.ReadFile(s)
			if err != nil {
				return err
			}
			if err := os.WriteFile(d, b, 0600); err != nil {
				return err
			}
		}
	}
	return nil
}
func (f *fakeBackend) DeleteSubvolume(_ context.Context, path string) error {
	if i, ok := f.sub(path); ok {
		f.subs = append(f.subs[:i], f.subs[i+1:]...)
	}
	return os.RemoveAll(path)
}
func (f *fakeBackend) Sync(context.Context, string) error                   { return nil }
func (f *fakeBackend) ValidateLayout(context.Context, string, string) error { return nil }
func (f *fakeBackend) ReadOnly(_ context.Context, path string) (bool, error) {
	i, ok := f.sub(path)
	if !ok {
		return false, os.ErrNotExist
	}
	return f.subs[i].ro, nil
}
func (f *fakeBackend) SetLimit(context.Context, string, int64) error { return nil }
func (f *fakeBackend) Mounted(context.Context, string) (bool, error) { return f.mounted, nil }
func (f *fakeBackend) Send(_ context.Context, path string, w io.Writer) error {
	files := map[string][]byte{}
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(path, p)
		b, err := os.ReadFile(p)
		files[rel] = b
		return err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(files)
}
func (f *fakeBackend) Receive(ctx context.Context, dir, name string, r io.Reader) error {
	f.received++
	var files map[string][]byte
	if err := json.NewDecoder(r).Decode(&files); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	if err := f.CreateSubvolume(ctx, dst); err != nil {
		return err
	}
	for rel, b := range files {
		if !streamPath(rel) {
			return errors.New("invalid fake stream")
		}
		p := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0600); err != nil {
			return err
		}
	}
	i, _ := f.sub(dst)
	f.subs[i].ro = true
	return nil
}

type fakeDatastore struct{}

func (fakeDatastore) Inspect(_ context.Context, root, k3s string) (Datastore, error) {
	b, err := os.ReadFile(filepath.Join(root, "database"))
	if err != nil {
		return Datastore{}, err
	}
	mapping, err := os.ReadFile(filepath.Join(root, "mapping"))
	if err != nil {
		return Datastore{}, err
	}
	return Datastore{Kind: "sqlite", AdapterVersion: 1, K3s: k3s, MappingSHA256: sum(mapping), Files: map[string]string{"database": sum(b)}}, nil
}

type fixture struct {
	e  *Engine
	fs *fakeBackend
	t  *testing.T
}

func setup(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	lib := filepath.Join(root, "lib")
	pool := filepath.Join(lib, "btrfs-csi-pool")
	fs := &fakeBackend{}
	ctx := context.Background()
	for _, p := range []string{lib, pool, filepath.Join(pool, "volumes/v1"), filepath.Join(pool, "snapshots/s1"), filepath.Join(lib, "rancher/k3s/agent/containerd"), filepath.Join(lib, "containers-anywhere/journal")} {
		if err := fs.CreateSubvolume(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Root: filepath.Join(root, "snapshots/recovery"), VarLib: lib, PoolDir: pool, Appliance: "0.1.0", K3s: "1.35.8+k3s1", DataFormat: "test-format", Architecture: "amd64", FS: fs, Datastore: fakeDatastore{}, Cold: func(context.Context) error { return nil }, Copy: func(_ context.Context, s, d string) error { return fs.copy(s, d, false) }}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{e, fs, t}
	f.write("containers-anywhere/data-format.json", `{"contract":"test-format"}`)
	f.write("database", "original-db")
	f.write("mapping", "pv=pvc=v1")
	f.write("btrfs-csi-pool/volumes/v1/data", "original-volume")
	f.write("btrfs-csi-pool/snapshots/s1/data", "snapshot-volume")
	f.write("btrfs-csi-pool/state.json", `{"volumes":{"v1":{"ID":"v1","Name":"pvc-pvcuid","CapacityBytes":4096}},"snapshots":{"s1":{"ID":"s1","Name":"snapshot-snapshotuid","SourceVolID":"v1","ReadyToUse":true}}}`)
	f.write("rancher/k3s/agent/containerd/cache", "excluded")
	return f
}
func (f fixture) write(rel, value string) {
	f.t.Helper()
	p := filepath.Join(f.e.cfg.VarLib, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value), 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f fixture) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.e.cfg.VarLib, rel))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}
func (f fixture) capture(uid, scope string) Manifest {
	f.t.Helper()
	op, err := f.e.Submit(context.Background(), Request{ID: uid, Action: "capture", Scope: scope})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.e.ProcessCold(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	st, err := f.e.Status(uid)
	if err != nil || st.Phase != "completed" {
		f.t.Fatalf("capture: %+v %v", st, err)
	}
	m, err := f.e.Get(op.PointID)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func TestColdCaptureSetAndDurableUID(t *testing.T) {
	f := setup(t)
	f.write("containers-anywhere/session", "running")
	ctx := context.Background()
	req := Request{ID: "kube-uid", Action: "capture", Scope: "full"}
	op, err := f.e.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Submit(ctx, Request{ID: "other", Action: "capture", Scope: "full"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected busy: %v", err)
	}
	reopened, err := New(f.e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	same, err := reopened.Submit(ctx, req)
	if err != nil || same.PointID != op.PointID {
		t.Fatal("durable UID lost", err)
	}
	if err := reopened.ProcessCold(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := reopened.Get(op.PointID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Captures, []string{"pool", "snap-s1", "system", "vol-v1"}) || m.GracefulShutdown || m.Consistency != "cold-crash-consistent" {
		t.Fatalf("bad manifest: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(reopened.pointPath(m.ID), "system/rancher/k3s/agent/containerd/cache")); !os.IsNotExist(err) {
		t.Fatal("nested cache leaked into system point")
	}
	req.Scope = "system"
	if _, err := reopened.Submit(ctx, req); err == nil {
		t.Fatal("UID reuse changed operation")
	}
}

func TestCaptureFailureCannotPublishPartialPoint(t *testing.T) {
	f := setup(t)
	f.fs.failSnapshot = "snap-s1"
	op, err := f.e.Submit(context.Background(), Request{ID: "fail", Action: "capture", Scope: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.ProcessCold(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := f.e.Status("fail")
	if st.Phase != "failed" {
		t.Fatal(st)
	}
	if _, err := f.e.Get(op.PointID); !os.IsNotExist(err) {
		t.Fatal("partial point published")
	}
	if f.read("database") != "original-db" {
		t.Fatal("capture changed database")
	}
}

func TestCorruptJournalFailsClosed(t *testing.T) {
	f := setup(t)
	_, err := f.e.Submit(context.Background(), Request{ID: "bad", Action: "capture", Scope: "full"})
	if err != nil {
		t.Fatal(err)
	}
	p := f.e.operationPath("bad")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte("restart-needed"), []byte("restoring"), 1)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.e.ProcessCold(context.Background()); !errors.Is(err, ErrInconsistent) {
		t.Fatalf("journal corruption allowed boot: %v", err)
	}
	if f.read("database") != "original-db" {
		t.Fatal("data changed")
	}
}

func TestSystemRestoreRejectsChangedMapping(t *testing.T) {
	f := setup(t)
	m := f.capture("system-point", "system")
	f.write("mapping", "pv=pvc=new-volume")
	f.write("database", "new-db")
	_, err := f.e.Submit(context.Background(), Request{ID: "restore", Action: "restore", Scope: "system", PointID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.ProcessCold(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, _ := f.e.Status("restore")
	if op.Phase != "failed" || !strings.Contains(op.Error, "mapping") {
		t.Fatal(op)
	}
	if f.read("database") != "new-db" {
		t.Fatal("mismatched system restore changed data")
	}
}

func TestFullRestoreReconcilesSnapshotsAndKeepsSafety(t *testing.T) {
	f := setup(t)
	m := f.capture("full-point", "full")
	f.write("database", "new-db")
	f.write("btrfs-csi-pool/volumes/v1/data", "new-volume")
	f.write("btrfs-csi-pool/snapshots/s1/data", "new-snapshot")
	ctx := context.Background()
	_, err := f.e.Submit(ctx, Request{ID: "restore", Action: "restore", Scope: "full", PointID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.ProcessCold(ctx); err != nil {
		t.Fatal(err)
	}
	op, _ := f.e.Status("restore")
	if op.Phase != "completed" || op.SafetyID == "" {
		t.Fatal(op)
	}
	if f.read("database") != "original-db" || f.read("btrfs-csi-pool/volumes/v1/data") != "original-volume" || f.read("btrfs-csi-pool/snapshots/s1/data") != "snapshot-volume" {
		t.Fatal("full point not restored")
	}
	b, err := os.ReadFile(filepath.Join(f.e.pointPath(op.SafetyID), "system/database"))
	if err != nil || string(b) != "new-db" {
		t.Fatal("safety not retained", err)
	}
	if err := f.e.Delete(ctx, m.ID); err == nil {
		t.Fatal("source deleted")
	}
	if err := f.e.Delete(ctx, op.SafetyID); err == nil {
		t.Fatal("safety deleted")
	}
}

func TestInterruptedDestructiveRestoreResumesWithoutLiveMarker(t *testing.T) {
	f := setup(t)
	m := f.capture("point", "full")
	f.write("database", "new-db")
	_, err := f.e.Submit(context.Background(), Request{ID: "restore", Action: "restore", Scope: "full", PointID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	original := f.e.cfg.Copy
	f.e.cfg.Copy = func(context.Context, string, string) error { panic("power-loss") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fault was not injected")
			}
		}()
		_ = f.e.ProcessCold(context.Background())
	}()
	if _, err := os.Stat(filepath.Join(f.e.cfg.VarLib, "containers-anywhere/data-format.json")); !os.IsNotExist(err) {
		t.Fatal("expected partial live tree")
	}
	op, _ := f.e.Status("restore")
	if op.Phase != "restoring" {
		t.Fatal(op)
	}
	f.e.cfg.Copy = original
	reopened, err := New(f.e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.ProcessCold(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, _ = reopened.Status("restore")
	if op.Phase != "completed" || f.read("database") != "original-db" {
		t.Fatal("restore did not resume", op)
	}
}

func TestRollbackJournalResumesAfterBothCopiesFail(t *testing.T) {
	f := setup(t)
	m := f.capture("point", "full")
	f.write("database", "new-db")
	_, err := f.e.Submit(context.Background(), Request{ID: "restore", Action: "restore", Scope: "full", PointID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	original := f.e.cfg.Copy
	f.e.cfg.Copy = func(context.Context, string, string) error { return errors.New("disk full") }
	if err := f.e.ProcessCold(context.Background()); !errors.Is(err, ErrInconsistent) {
		t.Fatalf("must block boot: %v", err)
	}
	op, _ := f.e.Status("restore")
	if op.Phase != "rolling-back" {
		t.Fatal(op)
	}
	f.e.cfg.Copy = original
	if err := f.e.ProcessCold(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, _ = f.e.Status("restore")
	if op.Phase != "failed" || f.read("database") != "new-db" {
		t.Fatal("rollback failed", op)
	}
}

func TestMissingSnapshotAndDriverInventoryRefuseRestore(t *testing.T) {
	f := setup(t)
	m := f.capture("point", "full")
	if err := f.fs.DeleteSubvolume(context.Background(), filepath.Join(f.e.pointPath(m.ID), "snap-s1")); err != nil {
		t.Fatal(err)
	}
	f.write("database", "new-db")
	_, err := f.e.Submit(context.Background(), Request{ID: "restore", Action: "restore", Scope: "full", PointID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.ProcessCold(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, _ := f.e.Status("restore")
	if op.Phase != "failed" || op.SafetyID != "" || f.read("database") != "new-db" {
		t.Fatal(op)
	}
	f.write("btrfs-csi-pool/state.json", `{"volumes":{},"snapshots":{}}`)
	if _, err := f.e.inventory(context.Background()); err == nil {
		t.Fatal("driver state disagrees with volumes but was accepted")
	}
}

func TestPortableExportImportAndCorruption(t *testing.T) {
	f := setup(t)
	m := f.capture("point", "full")
	var buf bytes.Buffer
	if err := f.e.Export(context.Background(), m.ID, &buf); err != nil {
		t.Fatal(err)
	}
	other := setup(t)
	got, err := other.e.Import(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatal("manifest changed on import")
	}
	if other.fs.received != len(m.Captures) {
		t.Fatal("incomplete receive")
	}
	third := setup(t)
	bad := append([]byte(nil), buf.Bytes()...)
	i := bytes.Index(bad, []byte("b3JpZ2luYWwtZGI="))
	if i < 0 {
		t.Fatal("test stream not found")
	}
	bad[i] = 'X'
	if _, err := third.e.Import(context.Background(), bytes.NewReader(bad)); err == nil {
		t.Fatal("corruption imported")
	}
	if third.fs.received != 0 {
		t.Fatal("received before authenticating all archive members")
	}
	if points, _ := third.e.List(); len(points) != 0 {
		t.Fatal("corrupt point published")
	}
}

func TestWorkloadRegistrationChecksActualCSIIdentity(t *testing.T) {
	f := setup(t)
	i, _ := f.fs.sub(filepath.Join(f.e.cfg.PoolDir, "snapshots/s1"))
	f.fs.subs[i].ro = true
	req := RegisterRequest{ID: "workload", Members: []SnapshotMember{{PVCNamespace: "ns", PVCName: "data", PVCUID: "pvcuid", PVName: "pvc-pvcuid", VolumeHandle: "v1", SnapshotHandle: "s1", SnapshotUID: "snapshotuid", SnapshotContentUID: "contentuid", Driver: "btrfs.csi.local"}}}
	m, err := f.e.RegisterSnapshots(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Captures, []string{"snap-s1"}) || m.Scope != "workload" {
		t.Fatal(m)
	}
	req.ID = "forged"
	req.Members[0].PVCUID = "someone-else"
	if _, err := f.e.RegisterSnapshots(context.Background(), req); err == nil {
		t.Fatal("forged owner registered")
	}
	if _, err := f.e.Submit(context.Background(), Request{ID: "activation", Action: "restore", Scope: "full", PointID: m.ID}); err == nil {
		t.Fatal("workload point used as appliance restore")
	}
}

func TestWorkloadRegistrationUsesSnapshotUIDNotContentUID(t *testing.T) {
	f := setup(t)
	i, _ := f.fs.sub(filepath.Join(f.e.cfg.PoolDir, "snapshots/s1"))
	f.fs.subs[i].ro = true
	member := SnapshotMember{PVCNamespace: "ns", PVCName: "data", PVCUID: "pvcuid", PVName: "pvc-pvcuid", VolumeHandle: "v1", SnapshotHandle: "s1", SnapshotUID: "snapshotuid", SnapshotContentUID: "different-content-uid", Driver: "btrfs.csi.local"}
	m, err := f.e.RegisterSnapshots(context.Background(), RegisterRequest{ID: "distinct-uids", Members: []SnapshotMember{member}})
	if err != nil {
		t.Fatal(err)
	}
	if m.Members[0].SnapshotUID != "snapshotuid" || m.Members[0].SnapshotContentUID != "different-content-uid" {
		t.Fatal("snapshot and content identities were conflated")
	}
	member.SnapshotUID = member.SnapshotContentUID
	if _, err := f.e.RegisterSnapshots(context.Background(), RegisterRequest{ID: "content-as-snapshot", Members: []SnapshotMember{member}}); err == nil {
		t.Fatal("content UID incorrectly accepted as the CSI snapshot name suffix")
	}
	member.SnapshotUID = ""
	if _, err := f.e.RegisterSnapshots(context.Background(), RegisterRequest{ID: "missing-snapshot-uid", Members: []SnapshotMember{member}}); err == nil {
		t.Fatal("missing VolumeSnapshot UID accepted")
	}
}

func TestReadOnlyPointDeletion(t *testing.T) {
	f := setup(t)
	m := f.capture("delete-point", "full")
	if err := f.e.Delete(context.Background(), m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Get(m.ID); !os.IsNotExist(err) {
		t.Fatal("point still published")
	}
}
