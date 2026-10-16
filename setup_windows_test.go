package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingSetupRestart(t *testing.T) {
	state := t.TempDir()
	if pendingSetupRestart(state) {
		t.Fatal("new installation requires restart")
	}
	path := filepath.Join(state, "setup-restart.json")
	if err := saveJSON(path, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !pendingSetupRestart(state) {
		t.Fatal("forgot pending restart")
	}
	if err := saveJSON(path, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if pendingSetupRestart(state) {
		t.Fatal("restart marker survived subsequent boot")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("did not clear completed restart marker")
	}
}
