package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBackupRoundTripAndRefuseOverwrite(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "clinic")
	os.Mkdir(state, 0700)
	original := []byte("synthetic clinic disk contents")
	os.WriteFile(filepath.Join(state, "data.img"), original, 0600)
	m, _ := json.Marshal(manifest{Version: "test", Arch: runtime.GOARCH})
	os.WriteFile(filepath.Join(state, "release.json"), m, 0600)
	file := filepath.Join(root, "backup.age")
	unlock, e := lockState(state)
	if e != nil {
		t.Fatal(e)
	}
	if e = backup(state, file); e == nil {
		t.Fatal("backed up active clinic")
	}
	unlock()
	if e = backup(state, file); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(root, "restored")
	if e = restore(target, file, file+".key"); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(target, "data.img"))
	if e != nil || string(got) != string(original) {
		t.Fatal("round trip failed", e)
	}
	if e = restore(target, file, file+".key"); e == nil {
		t.Fatal("overwrote existing data")
	}
	encrypted, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	encrypted[len(encrypted)-1] ^= 1
	bad := filepath.Join(root, "bad.age")
	os.WriteFile(bad, encrypted, 0600)
	broken := filepath.Join(root, "broken")
	if e = restore(broken, bad, file+".key"); e == nil {
		t.Fatal("accepted corruption")
	}
	if _, e = os.Stat(filepath.Join(broken, "data.img")); !os.IsNotExist(e) {
		t.Fatal("published corrupt data")
	}
}

func TestBundleVerifiesAdditionalRuntime(t *testing.T) {
	root, m := testBundle(t)
	if _, err := verifyBundle(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "system.img"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBundle(root); err == nil {
		t.Fatal("accepted modified runtime")
	}
	m.Files["../escape"] = "ignored"
	if err := saveJSON(filepath.Join(root, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBundle(root); err == nil {
		t.Fatal("accepted additional path traversal")
	}
}

func TestRestoreRefusesUnrelatedExistingFiles(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "important.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restore(state, "unused.age", "unused.key"); err == nil || err.Error() != "restore requires an empty destination" {
		t.Fatal(err)
	}
}

func TestBundlePaths(t *testing.T) {
	root := t.TempDir()
	m := manifest{Version: "test", Arch: runtime.GOARCH, System: "/nix/store/test", Kernel: "../escape", Initrd: "initrd"}
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(root, "manifest.json"), b, 0600)
	if _, e := verifyBundle(root); e == nil {
		t.Fatal("accepted traversal")
	}
}
