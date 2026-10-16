//go:build linux || windows

package main

import "testing"

func TestQemuPlatform(t *testing.T) {
	t.Setenv("CONTAINERS_QEMU_MACHINE", "")
	for _, tc := range []struct{ name, accel, arch, console, machine, cpu string }{
		{"kvm", "kvm", "amd64", "ttyS0", "q35", "host"},
		{"whpx", "whpx", "amd64", "ttyS0", "pc", "host"},
		{"tcg", "tcg", "amd64", "ttyS0", "q35", "max"},
		{"kvm arm64", "kvm", "arm64", "ttyAMA0", "virt", "host"},
	} {
		console, machine, cpu := qemuPlatform(tc.accel, tc.arch)
		if console != tc.console || machine != tc.machine || cpu != tc.cpu {
			t.Fatalf("%s: got %s/%s/%s, want %s/%s/%s", tc.name, console, machine, cpu, tc.console, tc.machine, tc.cpu)
		}
	}
	t.Setenv("CONTAINERS_QEMU_MACHINE", "pc")
	if _, machine, cpu := qemuPlatform("kvm", "amd64"); machine != "pc" || cpu != "host" {
		t.Fatal("override must select the machine without changing the CPU model")
	}
}
