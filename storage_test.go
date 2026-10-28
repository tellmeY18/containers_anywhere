package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestASparseDiskIsBackedUpAsItsDataAndRestoredWithItsHoles(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	os.Mkdir(state, 0700)
	const size = 64 << 20
	f, err := os.Create(filepath.Join(state, "data.img"))
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(size)
	head, mid, tail := []byte("head-of-disk"), []byte("middle-of-disk"), []byte("end-of-disk")
	f.WriteAt(head, 0)
	f.WriteAt(mid, 32<<20)
	f.WriteAt(tail, size-int64(len(tail)))
	f.Close()
	m, _ := json.Marshal(manifest{Version: "test", Arch: runtime.GOARCH})
	os.WriteFile(filepath.Join(state, "release.json"), m, 0600)
	file := filepath.Join(root, "backup.age")
	if err := backup(state, file); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(file)
	if info.Size() > 8<<20 {
		t.Skipf("this filesystem does not report holes (archive is %d bytes)", info.Size())
	}
	target := filepath.Join(root, "restored")
	if err := restore(target, file, file+".key"); err != nil {
		t.Fatal(err)
	}
	got, err := os.Open(filepath.Join(target, "data.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if st, _ := got.Stat(); st.Size() != size {
		t.Fatalf("restored size %d, want %d", st.Size(), size)
	}
	for off, want := range map[int64][]byte{0: head, 32 << 20: mid, size - int64(len(tail)): tail} {
		b := make([]byte, len(want))
		got.ReadAt(b, off)
		if string(b) != string(want) {
			t.Fatalf("data at %d: %q", off, b)
		}
	}
	if alloc, err := allocatedBytes(filepath.Join(target, "data.img")); err == nil && alloc > size/2 {
		t.Fatalf("the restore materialised the holes: %d bytes allocated", alloc)
	}
}

func TestSparseMapsAreValidated(t *testing.T) {
	good := sparseMap{Version: 1, Size: 100, Extents: []extent{{0, 10}, {50, 10}}}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]sparseMap{
		"overlap":      {Version: 1, Size: 100, Extents: []extent{{0, 20}, {10, 10}}},
		"past the end": {Version: 1, Size: 100, Extents: []extent{{95, 10}}},
		"zero length":  {Version: 1, Size: 100, Extents: []extent{{5, 0}}},
		"unsorted":     {Version: 1, Size: 100, Extents: []extent{{50, 10}, {0, 10}}},
		"huge disk":    {Version: 1, Size: maxDataDiskBytes + 1},
		"wrong ver":    {Version: 2, Size: 100},
	} {
		if m.validate() == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	f, _ := os.CreateTemp(t.TempDir(), "d")
	defer f.Close()
	if err := readSparseDisk(f, strings.NewReader(strings.Repeat("x", 10)), good, 5); err == nil {
		t.Fatal("a map that does not match the archived data must be refused")
	}
}
