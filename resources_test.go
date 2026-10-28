package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

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

func TestDataDiskGrowthKeepsAFreeFloorAndTheCeiling(t *testing.T) {
	state := t.TempDir()
	if err := diskGrowthRefusal(state, maxDataDiskBytes+1, 1<<30); err == nil {
		t.Fatal("growth above the ceiling must be refused")
	}
	if err := diskGrowthRefusal(state, 2<<30, 1<<30); err != nil {
		t.Fatalf("a small growth on a normal disk must be allowed: %v", err)
	}
	// Growth that would eat the floor is refused with the reason.
	err := diskGrowthRefusal(state, maxDataDiskBytes, 1<<30)
	if u, e := diskspace.Of(state); e == nil && int64(u.Free)-(maxDataDiskBytes-1<<30) < freeFloor(u.Total) {
		if err == nil || !strings.Contains(err.Error(), "at least") {
			t.Fatalf("growth that would leave less than the floor must be refused: %v", err)
		}
	}
	if freeFloor(100<<30) != 20<<30 || freeFloor(1000<<30) != 100<<30 {
		t.Fatal("the floor is the larger of 20 GiB and a tenth of the volume")
	}
}

func TestDataDiskFactReportsRealCostAndHeadroom(t *testing.T) {
	state := t.TempDir()
	f, _ := os.Create(filepath.Join(state, "data.img"))
	f.Truncate(8 << 30)
	f.WriteAt([]byte("x"), 0)
	f.Close()
	d := dataDiskFact(state)
	if d == nil || d.SizeBytes != 8<<30 || d.CeilingBytes != maxDataDiskBytes {
		t.Fatalf("%+v", d)
	}
	if d.AllocatedBytes <= 0 || d.AllocatedBytes >= 8<<30 {
		t.Fatalf("a sparse disk must cost much less than its size: %+v", d)
	}
	if d.MaxGrowthBytes > maxDataDiskBytes-8<<30 {
		t.Fatalf("headroom cannot exceed the ceiling: %+v", d)
	}
	if dataDiskFact(t.TempDir()) != nil {
		t.Fatal("no disk, no fact")
	}
}
