package recovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNeedsRestoreResumeDoesNotInitializeCatalog(t *testing.T) {
	root := filepath.Join(t.TempDir(), "recovery")
	if resume, err := NeedsRestoreResume(root); err != nil || resume {
		t.Fatalf("missing catalog: resume=%v err=%v", resume, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("preflight created its root: %v", err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if resume, err := NeedsRestoreResume(root); err != nil || resume {
		t.Fatalf("empty interrupted initialization: resume=%v err=%v", resume, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("preflight initialized an empty catalog: %v %v", entries, err)
	}
}

func TestOnlyDestructiveRestoreBypassesInitialFormatGuard(t *testing.T) {
	for _, tc := range []struct {
		action, phase string
		wantResume    bool
	}{
		{"capture", "restart-needed", false},
		{"capture", "capturing", false},
		{"capture", "completed", false},
		{"restore", "restart-needed", false},
		{"restore", "restoring", true},
		{"restore", "rolling-back", true},
		{"restore", "completed", false},
		{"restore", "failed", false},
	} {
		t.Run(tc.action+"-"+tc.phase, func(t *testing.T) {
			root := t.TempDir()
			op := resumeOperation(tc.action, tc.phase, "request-uid")
			path := filepath.Join(root, "operations", key(op.Request.UID)+".json")
			if err := writeRecord(path, op); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if resume, err := NeedsRestoreResume(root); err != nil || resume != tc.wantResume {
				t.Fatalf("resume=%v want=%v err=%v", resume, tc.wantResume, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("preflight modified the operation: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "operations" {
				t.Fatalf("preflight created a lock/catalog directory: %v %v", entries, err)
			}
		})
	}
}

func TestResumePreflightFailsClosedForCorruptionAndAmbiguity(t *testing.T) {
	for _, kind := range []string{"checksum", "phase", "capture-restoring", "missing-safety", "two-active", "lost-journals"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			op := resumeOperation("restore", "restoring", "request-uid")
			path := filepath.Join(root, "operations", key(op.Request.UID)+".json")
			switch kind {
			case "phase":
				op.Phase = "unknown-phase"
			case "capture-restoring":
				op.Request.Action = "capture"
				op.Request.PointID = ""
			case "missing-safety":
				op.SafetyID = ""
			}
			if err := writeRecord(path, op); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "checksum":
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				b = bytes.Replace(b, []byte("restoring"), []byte("completed"), 1)
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "two-active":
				other := resumeOperation("capture", "restart-needed", "second-request")
				if err := writeRecord(filepath.Join(root, "operations", key(other.Request.UID)+".json"), other); err != nil {
					t.Fatal(err)
				}
			case "lost-journals":
				if err := os.RemoveAll(filepath.Join(root, "operations")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "points"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if resume, err := NeedsRestoreResume(root); resume || !errors.Is(err, ErrInconsistent) {
				t.Fatalf("ambiguous journal bypassed guard: resume=%v err=%v", resume, err)
			}
			if _, err := os.Lstat(filepath.Join(root, "lock")); !os.IsNotExist(err) {
				t.Fatalf("corruption handling created a lock: %v", err)
			}
		})
	}
}

func resumeOperation(action, phase, uid string) Operation {
	op := Operation{Version: Version, Request: Request{ID: uid, UID: uid, Action: action, Scope: "full"}, Phase: phase, PointID: key(uid)}
	if action == "restore" {
		op.PointID = "point-source"
		op.Request.PointID = op.PointID
		op.SafetyID = "safety-" + key(uid)
	}
	return op
}
