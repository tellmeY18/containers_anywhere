package main

import "testing"

func TestUniversalResourceDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		cpus, memory         string
		wantCPUs, wantMemory int
		fail                 bool
	}{
		{"", "", 2, 4096, false},
		{"4", "8192", 4, 8192, false},
		{"2", "2048", 2, 2048, false},
		{"1", "4096", 0, 0, true},
		{"2", "1024", 0, 0, true},
		{"invalid", "", 0, 0, true},
	} {
		t.Setenv("CONTAINERS_CPUS", tc.cpus)
		t.Setenv("CONTAINERS_MEMORY_MIB", tc.memory)
		cpus, memory, err := vmResources()
		if (err != nil) != tc.fail || cpus != tc.wantCPUs || memory != tc.wantMemory {
			t.Fatalf("%+v: got %d CPUs, %d MiB, %v", tc, cpus, memory, err)
		}
	}
}
