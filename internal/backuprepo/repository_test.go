package backuprepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

func fixtureSpace(path string) (diskspace.Usage, error) {
	return diskspace.Usage{Path: path, Total: 100 << 30, Free: 80 << 30}, nil
}

func realTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testRepository(t *testing.T, keep int) (*Repository, string) {
	t.Helper()
	base := realTemp(t)
	state, dir, keys := filepath.Join(base, "state"), filepath.Join(base, "repo"), filepath.Join(base, "keys")
	for _, p := range []string{state, dir, keys} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(keys, "recovery.key")
	d, err := Enroll(state, EnrollmentOptions{Name: "USB backup", Path: dir, KeyPath: key, KeepLast: keep})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(state, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.DiskSpace = fixtureSpace
	t.Cleanup(func() { r.Close() })
	return r, key
}

func put(t *testing.T, r *Repository, id, payload string, protected bool) Entry {
	t.Helper()
	e, err := r.Export(context.Background(), strings.NewReader(payload), ExportOptions{ID: id, PointID: "point-" + id, Scope: "workload", ExpectedBytes: int64(len(payload)), Protected: protected})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestIndependentRecoveryWithoutOriginalStateOrGuest(t *testing.T) {
	r, key := testRepository(t, 8)
	payload := strings.Repeat("non-regenerable-volume-record\x00", 10000)
	e := put(t, r, "complete", payload, false)
	if e.Mode != "Full" || e.Transport != TransportFull || e.Scope != "workload" {
		t.Fatalf("metadata: %+v", e)
	}
	cipher, err := os.ReadFile(filepath.Join(r.Path, "complete.age"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte("non-regenerable-volume-record")) {
		t.Fatal("plaintext in encrypted repository")
	}
	if err := os.RemoveAll(filepath.Join(filepath.Dir(r.Path), "state")); err != nil {
		t.Fatal(err)
	}
	// Copy only repository files to another installation. No enrollment, data
	// disk, runtime, Kubernetes endpoint, or live guest is needed for verification.
	replacement := realTemp(t)
	files, err := os.ReadDir(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name() == ".repository.lock" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.Path, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(replacement, f.Name()), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	other, err := Open(replacement)
	if err != nil {
		t.Fatal(err)
	}
	other.Config.DiskSpace = fixtureSpace
	defer other.Close()
	got, err := other.Verify(context.Background(), e.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	if got.VerifiedAt == nil {
		t.Fatal("verification not recorded")
	}
	staging := realTemp(t)
	stage, err := other.Stage(context.Background(), e.ID, key, staging)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(stage.File)
	if err != nil || string(plain) != payload {
		t.Fatalf("payload mismatch: %v", err)
	}
	path := stage.Path
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plaintext stage survives close: %v", err)
	}
}

func TestTruncatedAndTamperedAgeNeverPublishPlaintext(t *testing.T) {
	for _, mode := range []string{"truncated", "tampered", "wrong-key", "catalog-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			r, key := testRepository(t, 8)
			e := put(t, r, "one", strings.Repeat("sensitive", 30000), false)
			name := filepath.Join(r.Path, e.ID+".age")
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			c, err := r.load()
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "truncated":
				b = b[:len(b)-17]
			case "tampered":
				b[len(b)-1] ^= 1
			case "wrong-key":
				identity, err := age.GenerateX25519Identity()
				if err != nil {
					t.Fatal(err)
				}
				key = filepath.Join(realTemp(t), "wrong.key")
				if err := os.WriteFile(key, []byte(identity.String()), 0600); err != nil {
					t.Fatal(err)
				}
			case "catalog-mismatch":
				c.Entries[0].PointID = "forged-point"
			}
			if err := os.WriteFile(name, b, 0600); err != nil {
				t.Fatal(err)
			}
			// Update the unauthenticated catalog hash/length too: rejection must
			// still come from age or the authenticated metadata, not just SHA256.
			sum := sha256.Sum256(b)
			c.Entries[0].SHA256 = hex.EncodeToString(sum[:])
			c.Entries[0].CipherBytes = int64(len(b))
			if err := writeJSON(r.root, catalogName, c, false); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Verify(context.Background(), e.ID, key); err == nil {
				t.Fatal("invalid archive verified")
			}
			staging := realTemp(t)
			if stage, err := r.Stage(context.Background(), e.ID, key, staging); err == nil {
				stage.Close()
				t.Fatal("invalid archive staged")
			}
			entries, err := os.ReadDir(staging)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed stage leaked plaintext: %v %v", entries, err)
			}
		})
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("guest export interrupted") }

func TestInterruptedExportPreservesPriorBackups(t *testing.T) {
	r, _ := testRepository(t, 1)
	put(t, r, "old", "old independent data", false)
	for _, mode := range []string{"read-error", "length-mismatch", "cancelled"} {
		ctx, cancel := context.WithCancel(context.Background())
		var src io.Reader = io.MultiReader(strings.NewReader("partial"), brokenReader{})
		expected := int64(-1)
		if mode == "length-mismatch" {
			src = strings.NewReader("short")
			expected = 100
		}
		if mode == "cancelled" {
			cancel()
			src = strings.NewReader("data")
		}
		_, err := r.Export(ctx, src, ExportOptions{ID: mode, PointID: "point", ExpectedBytes: expected})
		cancel()
		if err == nil {
			t.Fatalf("%s succeeded", mode)
		}
		entries, err := r.List()
		if err != nil || len(entries) != 1 || entries[0].ID != "old" {
			t.Fatalf("old backup changed: %v %v", entries, err)
		}
		if _, err := os.Stat(filepath.Join(r.Path, mode+".age")); !os.IsNotExist(err) {
			t.Fatalf("incomplete backup published: %v", err)
		}
	}
	files, err := os.ReadDir(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".pending-") {
			t.Fatalf("temporary leak: %s", f.Name())
		}
	}
}

func TestExclusivePublicationRefusesCatalogAndOrphanCollisions(t *testing.T) {
	r, _ := testRepository(t, 8)
	put(t, r, "exists", "original", false)
	for _, id := range []string{"exists", "orphan"} {
		if id == "orphan" {
			if err := os.WriteFile(filepath.Join(r.Path, id+".age"), []byte("unrelated"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		before, err := os.ReadFile(filepath.Join(r.Path, id+".age"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Export(context.Background(), strings.NewReader("overwrite"), ExportOptions{ID: id, PointID: "point", ExpectedBytes: -1})
		if !errors.Is(err, os.ErrExist) {
			t.Fatalf("collision: %v", err)
		}
		after, err := os.ReadFile(filepath.Join(r.Path, id+".age"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("backup overwritten")
		}
	}
}

func TestRepositoryOSLockSerializesIndependentHandles(t *testing.T) {
	r, _ := testRepository(t, 8)
	other, err := Open(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	other.Config.DiskSpace = fixtureSpace
	defer other.Close()
	unlock, err := acquire(r.root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.Export(context.Background(), strings.NewReader("data"), ExportOptions{ID: "blocked", PointID: "point", ExpectedBytes: -1})
	if !errors.Is(err, ErrLocked) {
		unlock()
		t.Fatalf("concurrent writer not excluded: %v", err)
	}
	if _, err := other.List(); !errors.Is(err, ErrLocked) {
		unlock()
		t.Fatalf("concurrent catalog read not excluded: %v", err)
	}
	unlock()
	put(t, other, "after-unlock", "data", false)
}

func TestRetentionProtectsPointsAndLastVerified(t *testing.T) {
	r, key := testRepository(t, 2)
	put(t, r, "protected", "first", true)
	put(t, r, "old", "second", false)
	put(t, r, "new", "third", false)
	entries, err := r.List()
	if err != nil || len(entries) != 2 || entries[0].ID != "protected" || entries[1].ID != "new" {
		t.Fatalf("retention: %+v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(r.Path, "old.age")); !os.IsNotExist(err) {
		t.Fatalf("old archive retained: %v", err)
	}
	if _, err := r.Verify(context.Background(), "new", key); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Export(context.Background(), strings.NewReader("fourth"), ExportOptions{ID: "blocked", PointID: "point", ExpectedBytes: -1}); err == nil {
		t.Fatal("retention exceeded protected capacity")
	}
	if err := r.Protect("protected", false); err != nil {
		t.Fatal(err)
	}
	put(t, r, "fourth", "fourth", false)
	entries, err = r.List()
	if err != nil || len(entries) != 2 || entries[0].ID != "new" {
		t.Fatalf("last verified point removed: %+v %v", entries, err)
	}
}

func TestPreflightReserveFailureDoesNotPrune(t *testing.T) {
	r, _ := testRepository(t, 1)
	put(t, r, "previous", "previous", false)
	r.Config.MinFreeBytes = ^uint64(0)
	_, err := r.Export(context.Background(), strings.NewReader("new"), ExportOptions{ID: "new", PointID: "point", ExpectedBytes: -1})
	if err == nil {
		t.Fatal("space reserve ignored")
	}
	entries, err := r.List()
	if err != nil || len(entries) != 1 || entries[0].ID != "previous" {
		t.Fatalf("previous backup lost: %v %v", entries, err)
	}
}

func TestTraversalSymlinksAndCatalogPathsRejected(t *testing.T) {
	r, key := testRepository(t, 8)
	for _, id := range []string{"../outside", "a/b", `a\b`, ".", "..", "C:escape", "/absolute", "CON", "nul", "COM1", "LPT9"} {
		if _, err := r.Export(context.Background(), strings.NewReader("data"), ExportOptions{ID: id, PointID: "point", ExpectedBytes: -1}); err == nil {
			t.Fatalf("unsafe id %q accepted", id)
		}
	}
	put(t, r, "safe", "payload", false)
	c, err := r.load()
	if err != nil {
		t.Fatal(err)
	}
	c.Entries[0].ID = "../escaped"
	if err := writeJSON(r.root, catalogName, c, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.List(); err == nil {
		t.Fatal("unsafe catalog path accepted")
	}
	if _, err := r.Stage(context.Background(), "safe", key, realTemp(t)); err == nil {
		t.Fatal("unsafe catalog staged")
	}
	link := filepath.Join(realTemp(t), "alias")
	if err := os.Symlink(r.Path, link); err != nil {
		t.Logf("symlink creation unavailable: %v", err)
		return
	}
	if _, err := Open(link); err == nil {
		t.Fatal("symlink repository accepted")
	}
}

func TestLinkedArchiveAndLinkedKeyRejected(t *testing.T) {
	r, key := testRepository(t, 8)
	put(t, r, "one", "secret payload", false)
	archive := filepath.Join(r.Path, "one.age")
	moved := filepath.Join(realTemp(t), "copied.age")
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(moved, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, archive); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := r.Verify(context.Background(), "one", key); err == nil {
		t.Fatal("linked archive verified")
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, b, 0600); err != nil {
		t.Fatal(err)
	}
	linkedKey := filepath.Join(realTemp(t), "linked.key")
	if err := os.Symlink(key, linkedKey); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(context.Background(), "one", linkedKey); err == nil {
		t.Fatal("linked key accepted")
	}
}

func TestEnrollmentRejectsStateOverlapRelativeAndTraversalPaths(t *testing.T) {
	base := realTemp(t)
	state := filepath.Join(base, "state")
	inside := filepath.Join(state, "repo")
	if err := os.MkdirAll(inside, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{inside, base, "relative", state + string(filepath.Separator) + ".."} {
		if _, err := Enroll(state, EnrollmentOptions{Name: "bad", Path: path, Recipient: identity.Recipient().String()}); err == nil {
			t.Fatalf("unsafe enrollment path %q accepted", path)
		}
	}
}

func TestMissingMountedRootNeverCreatesReplacement(t *testing.T) {
	r, _ := testRepository(t, 8)
	state := filepath.Join(filepath.Dir(r.Path), "state")
	id, path := r.Config.ID, r.Path
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-disconnected"); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(state, id); err == nil {
		t.Fatal("missing drive resolved")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("replacement folder created: %v", err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(state, id); err == nil {
		t.Fatal("empty mount-point replacement resolved")
	}
	names, err := os.ReadDir(path)
	if err != nil || len(names) != 0 {
		t.Fatalf("replacement populated: %v %v", names, err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	newRepo, err := Initialize(path, Config{Recipient: identity.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer newRepo.Close()
	if _, err := Resolve(state, id); err == nil {
		t.Fatal("different mounted repository resolved")
	}
}

func TestEnrollmentRequiresSeparateKeyOrPublicRecipient(t *testing.T) {
	base := realTemp(t)
	state, repo := filepath.Join(base, "state"), filepath.Join(base, "repo")
	for _, p := range []string{state, repo} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"", filepath.Join(repo, "key"), filepath.Join(state, "key")} {
		if _, err := Enroll(state, EnrollmentOptions{Name: "destination", Path: repo, KeyPath: key}); err == nil {
			t.Fatalf("unsafe key %q accepted", key)
		}
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Enroll(state, EnrollmentOptions{Name: "destination", Path: repo, Recipient: identity.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	if d.ID == "destination" || len(d.ID) != 32 {
		t.Fatalf("destination ID not opaque: %s", d.ID)
	}
	facts, err := Facts(state)
	if err != nil || len(facts) != 1 || !facts[0].Available {
		t.Fatalf("facts: %v %v", facts, err)
	}
	b, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), repo) || strings.Contains(string(b), identity.Recipient().String()) {
		t.Fatal("host enrollment leaked into cluster facts")
	}
	if err := Forget(state, d.ID); err != nil {
		t.Fatal(err)
	}
	remaining, err := Open(repo)
	if err != nil {
		t.Fatalf("forget removed repository: %v", err)
	}
	remaining.Close()
}
