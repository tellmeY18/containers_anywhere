package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testBundle(t *testing.T) (string, manifest) {
	t.Helper()
	dir := t.TempDir()
	m := manifest{Product: "containers-anywhere", Version: "0.1.0", Arch: runtime.GOARCH, Format: 3, Protocol: 1, DataBytes: 16 << 30, Kernel: "kernel", Initrd: "initrd", System: "/nix/store/test", Files: map[string]string{}}
	for _, name := range []string{"kernel", "initrd", "system.img", "data.img"} {
		b := []byte(name)
		m.Files[name] = sha256Text(b)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	return dir, m
}

func TestApplianceContracts(t *testing.T) {
	dir, m := testBundle(t)
	if _, err := verifyBundle(dir); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*manifest){
		func(m *manifest) { m.Format = 2 }, func(m *manifest) { m.Product = "care" },
		func(m *manifest) { m.Protocol++ }, func(m *manifest) { m.DataBytes = -1 },
	} {
		next := m
		mutate(&next)
		if validateLayers(next) == nil {
			t.Fatal("accepted incompatible appliance")
		}
	}
	if !compatibleData(m, m) {
		t.Fatal("same appliance rejected")
	}
	next := m
	next.System = "/nix/store/new"
	if compatibleData(m, next) {
		t.Fatal("silently upgraded cluster")
	}
	if err := os.WriteFile(filepath.Join(dir, "system.img"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBundle(dir); err == nil {
		t.Fatal("accepted tampered disk")
	}
}

func TestSmallSeedCreatesFullDiskWithoutOverwriting(t *testing.T) {
	bundle, m := testBundle(t)
	state := t.TempDir()
	m.DataBytes = 1 << 20
	if err := ensureData(state, bundle, m); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(state, "data.img"))
	if err != nil || info.Size() != m.DataBytes {
		t.Fatal(info, err)
	}
	if err = os.WriteFile(filepath.Join(state, "data.img"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = ensureData(state, bundle, m); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(state, "data.img"))
	if string(b) != "existing" {
		t.Fatal("reinitialized existing disk")
	}
}
