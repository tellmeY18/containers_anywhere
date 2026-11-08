package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryJournalBlocksRootChangesUntilMatchingCompletion(t *testing.T) {
	dir := t.TempDir()
	if err := recordRecoveryActivity(dir, "request-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := updateLock(dir, false); err == nil {
		t.Fatal("an update obtained a lock while cold recovery was pending")
	}
	if err := finishRecoveryActivity(dir, "request-2"); err == nil {
		t.Fatal("another operation cleared the active recovery")
	}
	if err := checkRecoveryIdle(dir); err == nil {
		t.Fatal("pending recovery was lost")
	}
	if err := finishRecoveryActivity(dir, "request-1"); err != nil {
		t.Fatal(err)
	}
	if err := checkRecoveryIdle(dir); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidRecoveryJournalCannotBeOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, recoveryActivityFile)
	if err := os.WriteFile(path, []byte("{truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recordRecoveryActivity(dir, "request-1"); err == nil {
		t.Fatal("corrupt recovery journal was overwritten")
	}
	if err := checkRecoveryIdle(dir); err == nil {
		t.Fatal("corrupt recovery journal was ignored")
	}
}
