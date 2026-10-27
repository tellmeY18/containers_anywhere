package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/protocol"
)

func testBundle(t *testing.T) (string, manifest) {
	t.Helper()
	dir := t.TempDir()
	m := manifest{Product: "containers-anywhere", Version: "0.1.0", Arch: runtime.GOARCH, Format: 3, Protocol: 1, DataBytes: 16 << 30, Kernel: "kernel", Initrd: "initrd", System: "/nix/store/test", Files: map[string]string{}}
	m.Capabilities = protocol.Capabilities()
	m.DataContract, m.K3s = "var-lib-v1", "1.35.0+k3s1"
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
		func(m *manifest) { m.Capabilities = []string{"status"} },
		func(m *manifest) { m.Capabilities = nil },
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
	next = m
	next.Version = "0.1.99"
	if err := validateLayers(next); err != nil {
		t.Fatal("protocol-compatible patch appliance rejected:", err)
	}
	if compatibleData(m, next) {
		t.Fatal("protocol compatibility must not authorize a disk migration")
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

func TestK3sChangeClassification(t *testing.T) {
	for _, tc := range []struct {
		old, next          string
		identical, upgrade bool
	}{
		{"1.35.8+k3s1", "1.35.8+k3s1", true, false},
		{"1.35.8+k3s1", "1.35.9+k3s1", false, true},
		{"1.35.8+k3s1", "1.35.8+k3s2", false, true},
		{"1.35.8+k3s2", "1.35.8+k3s1", false, false},
		{"1.35.9+k3s1", "1.35.8+k3s1", false, false},
		{"1.35.8+k3s1", "1.36.0+k3s1", false, false},
		{"1.35.8+k3s1", "2.35.8+k3s1", false, false},
		{"1.35.8+k3s1", "1.35.8", false, false},
		{"", "", false, false},
		{"new-schema", "new-schema", true, false},
	} {
		identical, upgrade := k3sChange(tc.old, tc.next)
		if identical != tc.identical || upgrade != tc.upgrade {
			t.Errorf("%q -> %q: identical=%v upgrade=%v, want %v %v", tc.old, tc.next, identical, upgrade, tc.identical, tc.upgrade)
		}
	}
}

func TestUpdateAllowsOnlyForwardK3sPatchWithinAMinor(t *testing.T) {
	_, m := testBundle(t)
	for next, ok := range map[string]bool{
		"1.35.0+k3s1": true, "1.35.4+k3s1": true, "1.35.0+k3s2": true,
		"1.34.9+k3s1": false, "1.36.0+k3s1": false, "1.35.0+k3s0": false,
	} {
		n := m
		n.K3s = next
		err := updateDataCompatible(m, n)
		if (err == nil) != ok {
			t.Errorf("K3s %s -> %s: err=%v, want ok=%v", m.K3s, next, err, ok)
		}
	}
}

func TestAnApplianceWithoutTheHostAPIStillRunsAndCanBeUpdated(t *testing.T) {
	_, old := testBundle(t)
	old.Capabilities = []string{"status", "logs", "stop", "console-proxy", "platform-apply-v1", "workload-publication-v1"}
	if err := validateLayers(old); err != nil {
		t.Fatalf("a published 0.2.0 appliance must stay launchable by a newer app: %v", err)
	}
	if hostAPISupported(old) {
		t.Fatal("the bridge must stay off for an appliance that does not advertise it")
	}
	next := old
	next.System = "/nix/store/newer"
	next.Capabilities = append(append([]string{}, old.Capabilities...), "host-api-v1")
	if err := updateDataCompatible(old, next); err != nil {
		t.Fatalf("updating from an earlier appliance to one with the host API must work: %v", err)
	}
	if err := swapEligible(old, next); err != nil {
		t.Fatalf("and so must a root swap: %v", err)
	}
	if !hostAPISupported(next) {
		t.Fatal("the new appliance advertises the host API")
	}
}
