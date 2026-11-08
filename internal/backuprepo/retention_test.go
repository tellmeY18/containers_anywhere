package backuprepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

func TestRetentionJournalRetriesOnlyExactDeletions(t *testing.T) {
	for _, retry := range []string{"prune", "export", "open"} {
		t.Run(retry, func(t *testing.T) {
			r, _ := testRepository(t, 1)
			put(t, r, "old", "old payload", false)
			orphan := filepath.Join(r.Path, "unrelated-orphan.age")
			if err := os.WriteFile(orphan, []byte("not in any catalog"), 0600); err != nil {
				t.Fatal(err)
			}
			r.removeFile = func(string) error { return errors.New("simulated deletion failure") }
			e, err := r.Export(context.Background(), strings.NewReader("new payload"), ExportOptions{ID: "new", PointID: "new-point", ExpectedBytes: -1})
			if err == nil || e.ID != "new" {
				t.Fatalf("expected committed backup with retention failure: %+v %v", e, err)
			}
			c, err := r.load()
			if err != nil || len(c.PendingDeletions) != 1 || c.PendingDeletions[0] != "old" || len(c.Entries) != 1 || c.Entries[0].ID != "new" {
				t.Fatalf("lost deletion journal: %+v %v", c, err)
			}
			if _, err := os.Stat(filepath.Join(r.Path, "old.age")); err != nil {
				t.Fatal(err)
			}
			r.removeFile = nil
			switch retry {
			case "prune":
				if err := r.Prune(); err != nil {
					t.Fatal(err)
				}
			case "export":
				put(t, r, "next", "next payload", false)
			case "open":
				other, err := Open(r.Path)
				if err != nil {
					t.Fatal(err)
				}
				other.Close()
			}
			c, err = r.load()
			if err != nil || len(c.PendingDeletions) != 0 {
				t.Fatalf("journal not acknowledged: %+v %v", c, err)
			}
			if _, err := os.Stat(filepath.Join(r.Path, "old.age")); !os.IsNotExist(err) {
				t.Fatalf("old ciphertext retained: %v", err)
			}
			b, err := os.ReadFile(orphan)
			if err != nil || string(b) != "not in any catalog" {
				t.Fatal("retention touched an unrelated export orphan")
			}
		})
	}
}

func TestRetentionResumesAfterDeletionBeforeAcknowledgement(t *testing.T) {
	r, _ := testRepository(t, 3)
	for _, id := range []string{"first", "second", "newest"} {
		put(t, r, id, id, false)
	}
	r.Config.KeepLast = 1
	calls := 0
	r.removeFile = func(name string) error {
		calls++
		if err := r.root.Remove(name); err != nil {
			return err
		}
		if calls == 1 {
			return errors.New("simulated crash after removing file")
		}
		return nil
	}
	if err := r.Prune(); err == nil {
		t.Fatal("expected interrupted deletion")
	}
	c, err := r.load()
	if err != nil || len(c.PendingDeletions) != 2 {
		t.Fatalf("journal lost after first unlink: %+v %v", c, err)
	}
	if _, err := r.root.Lstat("first.age"); !os.IsNotExist(err) {
		t.Fatal("fixture did not delete first file")
	}
	other, err := Open(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	c, err = other.load()
	if err != nil || len(c.PendingDeletions) != 0 || len(c.Entries) != 1 || c.Entries[0].ID != "newest" {
		t.Fatalf("retry did not acknowledge missing and remaining files: %+v %v", c, err)
	}
	if _, err := other.root.Lstat("second.age"); !os.IsNotExist(err) {
		t.Fatal("remaining exact deletion was not resumed")
	}
}

func TestRetentionRejectsUnsafeOrAdvertisedDeletionIDs(t *testing.T) {
	for _, id := range []string{"../outside", "a/b", "active", "duplicate"} {
		t.Run(id, func(t *testing.T) {
			r, _ := testRepository(t, 8)
			put(t, r, "active", "retained", true)
			c, err := r.load()
			if err != nil {
				t.Fatal(err)
			}
			c.PendingDeletions = []string{id}
			if id == "duplicate" {
				c.PendingDeletions = append(c.PendingDeletions, id)
			}
			if err := writeJSON(r.root, catalogName, c, false); err != nil {
				t.Fatal(err)
			}
			r.removeFile = func(name string) error { t.Fatalf("unsafe journal invoked deletion: %s", name); return nil }
			if err := r.Prune(); err == nil {
				t.Fatal("unsafe pending deletion accepted")
			}
			if other, err := Open(r.Path); err == nil {
				other.Close()
				t.Fatal("unsafe journal accepted on open")
			}
			if _, err := r.root.Lstat("active.age"); err != nil {
				t.Fatal("advertised backup was removed")
			}
		})
	}
}

func TestPerRepositoryCapacityIsDeterministicAndNotPersisted(t *testing.T) {
	r, _ := testRepository(t, 1)
	put(t, r, "previous", "prior independent backup", false)
	for _, failure := range []string{"preflight", "during-stream", "probe-error"} {
		calls := 0
		r.Config.DiskSpace = func(path string) (diskspace.Usage, error) {
			calls++
			if failure == "probe-error" {
				return diskspace.Usage{}, errors.New("injected capacity probe failure")
			}
			if failure == "preflight" || calls >= 3 {
				return diskspace.Usage{Path: path, Total: 100 << 30, Free: 19 << 30}, nil
			}
			return fixtureSpace(path)
		}
		_, err := r.Export(context.Background(), strings.NewReader(strings.Repeat("x", 3<<20)), ExportOptions{ID: failure, PointID: "new-point", ExpectedBytes: -1})
		if err == nil {
			t.Fatalf("%s capacity failure ignored", failure)
		}
		entries, err := r.List()
		if err != nil || len(entries) != 1 || entries[0].ID != "previous" {
			t.Fatalf("capacity failure changed retained backup: %+v %v", entries, err)
		}
		if _, err := r.root.Lstat(failure + ".age"); !os.IsNotExist(err) {
			t.Fatalf("failed export published: %v", err)
		}
	}
	r.Config.DiskSpace = fixtureSpace
	if err := writeJSON(r.root, markerName, r.Config, false); err != nil {
		t.Fatal(err)
	}
	other, err := Open(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Config.DiskSpace != nil {
		t.Fatal("test capacity override persisted into repository configuration")
	}
	other.Config.DiskSpace = func(path string) (diskspace.Usage, error) {
		return diskspace.Usage{Path: path, Total: 100 << 30, Free: 0}, nil
	}
	put(t, r, "independent", "independent provider", false)
	if _, err := other.Export(context.Background(), strings.NewReader("blocked"), ExportOptions{ID: "blocked", PointID: "point", ExpectedBytes: -1}); err == nil {
		t.Fatal("capacity override leaked between repository handles")
	}
}

func TestStagingCapacityFailureRemovesAllPlaintext(t *testing.T) {
	r, key := testRepository(t, 8)
	put(t, r, "complete", strings.Repeat("s", 3<<20), false)
	calls := 0
	r.Config.DiskSpace = func(path string) (diskspace.Usage, error) {
		calls++
		if calls >= 3 {
			return diskspace.Usage{Path: path, Total: 100 << 30, Free: 19 << 30}, nil
		}
		return fixtureSpace(path)
	}
	staging := realTemp(t)
	if stage, err := r.Stage(context.Background(), "complete", key, staging); err == nil {
		stage.Close()
		t.Fatal("staging ignored simulated capacity exhaustion")
	}
	files, err := os.ReadDir(staging)
	if err != nil || len(files) != 0 {
		t.Fatalf("staging capacity failure left plaintext: %+v %v", files, err)
	}
	entries, err := r.List()
	if err != nil || len(entries) != 1 || entries[0].ID != "complete" {
		t.Fatal("staging failure changed encrypted backup")
	}
}
