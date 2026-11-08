package backuprepo

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

func noHardlinks(string, string) error { return &os.LinkError{Op: "link", Err: errors.ErrUnsupported} }

func TestNoHardlinkExportIsCatalogPublishedAndIndependentlyVerified(t *testing.T) {
	r, key := testRepository(t, 8)
	r.linkFile = noHardlinks
	payload := strings.Repeat("full-export-unique-record", 10000)
	e := put(t, r, "copied", payload, false)
	if _, err := r.Verify(context.Background(), e.ID, key); err != nil {
		t.Fatal(err)
	}
	stage, err := r.Stage(context.Background(), e.ID, key, realTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	got, err := io.ReadAll(stage.File)
	if err != nil || string(got) != payload {
		t.Fatalf("copied archive mismatch: %v", err)
	}
}

func TestInterruptedCopyRemainsUnlistedAndNeverOverwritten(t *testing.T) {
	r, key := testRepository(t, 1)
	put(t, r, "previous", "previous independent backup", false)
	r.linkFile = noHardlinks
	r.copyFile = func(dst io.Writer, src io.Reader) (int64, error) {
		n, err := io.CopyN(dst, src, 32)
		if err != nil {
			return n, err
		}
		// The complete catalog must still show only the prior point while
		// the non-atomic destination file is being copied.
		c, err := r.load()
		if err != nil || len(c.Entries) != 1 || c.Entries[0].ID != "previous" {
			t.Fatalf("copy appeared completed before EOF: %v %v", c, err)
		}
		return n, errors.New("simulated unplug during fallback copy")
	}
	_, err := r.Export(context.Background(), strings.NewReader("new full export"), ExportOptions{ID: "partial", PointID: "new-point", ExpectedBytes: -1})
	if err == nil {
		t.Fatal("interrupted copy succeeded")
	}
	entries, err := r.List()
	if err != nil || len(entries) != 1 || entries[0].ID != "previous" {
		t.Fatalf("previous backup lost: %v %v", entries, err)
	}
	if _, err := r.Verify(context.Background(), "partial", key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial orphan accepted for verification: %v", err)
	}
	name := filepath.Join(r.Path, "partial.age")
	before, err := os.ReadFile(name)
	if err != nil || len(before) != 32 {
		t.Fatalf("partial orphan not retained: %d %v", len(before), err)
	}
	r.copyFile = nil
	if _, err := r.Export(context.Background(), strings.NewReader("retry"), ExportOptions{ID: "partial", PointID: "new-point", ExpectedBytes: -1}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("orphan overwritten on retry: %v", err)
	}
	after, err := os.ReadFile(name)
	if err != nil || string(after) != string(before) {
		t.Fatal("partial orphan changed")
	}
}

func TestCopyFallbackNeverTouchesExistingDestinationOrHidesOtherErrors(t *testing.T) {
	root, err := openRoot(realTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, tmp, err := temporary(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("staged bytes"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	to, err := root.OpenFile("existing", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := to.WriteString("unrelated data"); err != nil {
		t.Fatal(err)
	}
	to.Close()
	if err := publishWith(root, tmp, "existing", noHardlinks, io.Copy); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive copy did not refuse existing file: %v", err)
	}
	in, err := root.Open("existing")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(in)
	in.Close()
	if err != nil || string(b) != "unrelated data" {
		t.Fatal("unrelated destination changed")
	}
	for _, cause := range []error{os.ErrPermission, os.ErrNotExist, os.ErrExist, errors.New("device I/O error")} {
		copied := false
		err := publishWith(root, tmp, "not-created", func(string, string) error { return cause }, func(io.Writer, io.Reader) (int64, error) { copied = true; return 0, nil })
		if !errors.Is(err, cause) || copied {
			t.Fatalf("non-unsupported failure triggered fallback: %v", err)
		}
		if _, err := root.Lstat("not-created"); !os.IsNotExist(err) {
			t.Fatalf("destination created for unrelated error: %v", err)
		}
	}
}

func TestFailedKeyCopyRetainsCompletePrivateKeyAndPartialDestination(t *testing.T) {
	base := realTemp(t)
	state, repo, keys := filepath.Join(base, "state"), filepath.Join(base, "repo"), filepath.Join(base, "keys")
	for _, path := range []string{state, repo, keys} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(keys, "explicit.key")
	_, err := generateKeyWithPublish(state, repo, key, func(root *os.Root, from, to string) error {
		return publishWithCreate(root, from, to, noHardlinks, func(dst io.Writer, src io.Reader) (int64, error) {
			n, err := io.CopyN(dst, src, 10)
			if err != nil {
				return n, err
			}
			return n, errors.New("simulated key publication failure")
		}, createPrivateFile)
	})
	if err == nil || !strings.Contains(err.Error(), "complete key retained") {
		t.Fatalf("missing retained-key diagnostic: %v", err)
	}
	partial, err := os.ReadFile(key)
	if err != nil || len(partial) != 10 {
		t.Fatalf("explicit key destination unexpectedly removed: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(keys, ".recovery-key-*", ".pending-*"))
	if err != nil || len(stages) != 1 {
		t.Fatalf("complete key stage lost: %v %v", stages, err)
	}
	complete, err := os.ReadFile(stages[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := age.ParseX25519Identity(strings.TrimSpace(string(complete))); err != nil {
		t.Fatalf("retained key incomplete: %v", err)
	}
	if _, err := generateKey(state, repo, key); err == nil {
		t.Fatal("partial explicit key silently overwritten")
	}
}

func TestHostReserveReadinessAndOverride(t *testing.T) {
	for _, tt := range []struct{ total, configured, want uint64 }{
		{100 << 30, 0, 20 << 30}, {1000 << 30, 20 << 30, 100 << 30}, {100 << 30, 40 << 30, 40 << 30},
	} {
		if got := EffectiveReserve(tt.total, tt.configured); got != tt.want {
			t.Fatalf("reserve(%d,%d)=%d want %d", tt.total, tt.configured, got, tt.want)
		}
	}
	u := diskspace.Usage{Total: 100 << 30, Free: 21 << 30}
	if !spaceReady(u, 1<<30, 20<<30) || spaceReady(u, (1<<30)+1, 20<<30) {
		t.Fatal("reserve arithmetic did not preserve the floor")
	}
	if spaceReady(u, 0, ^uint64(0)) {
		t.Fatal("large reserve wrapped")
	}
	r, _ := testRepository(t, 8)
	r.Config.MinFreeBytes = ^uint64(0)
	if err := writeJSON(r.root, markerName, r.Config, false); err != nil {
		t.Fatal(err)
	}
	facts, err := Facts(filepath.Join(filepath.Dir(r.Path), "state"))
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts: %v %v", facts, err)
	}
	if !facts[0].Available || facts[0].Ready || facts[0].MinFreeBytes != ^uint64(0) || facts[0].ReserveBytes != ^uint64(0) {
		t.Fatalf("capacity readiness missing: %+v", facts[0])
	}
}
