package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
	"github.com/tellmeY18/containers_anywhere/internal/hostapi"
)

const approvedResourcesFile = "resources.json"

// approvedResources is a size an administrator approved in the launcher. It takes
// effect only when the appliance next starts, and never touches the data disk.
type approvedResources struct {
	CPUs       int    `json:"cpus"`
	MemoryMiB  int    `json:"memoryMiB"`
	DiskGiB    int    `json:"diskGiB,omitempty"`
	RequestUID string `json:"requestUID,omitempty"`
	ApprovedAt string `json:"approvedAt,omitempty"`
}

func validResources(cpus, memory int) error {
	if cpus < 2 || cpus > 64 || memory < 2048 || memory > 262144 {
		return fmt.Errorf("VM requires 2–64 CPUs and 2048–262144 MiB RAM")
	}
	return nil
}

func readApprovedResources(state string) (approvedResources, bool) {
	var a approvedResources
	b, err := os.ReadFile(filepath.Join(state, approvedResourcesFile))
	if err != nil || json.Unmarshal(b, &a) != nil || validResources(a.CPUs, a.MemoryMiB) != nil {
		return approvedResources{}, false
	}
	return a, true
}

// vmResourcesFor resolves the size: environment override, else the size approved
// in the launcher, else the defaults. An unreadable approved file is ignored
// rather than blocking boot; the defaults are always safe.
func vmResourcesFor(state string) (int, int, error) {
	values := []int{2, 4096}
	if state != "" {
		if a, ok := readApprovedResources(state); ok {
			values[0], values[1] = a.CPUs, a.MemoryMiB
		}
	}
	for i, name := range []string{"CONTAINERS_CPUS", "CONTAINERS_MEMORY_MIB"} {
		if s := os.Getenv(name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				return 0, 0, fmt.Errorf("%s must be an integer", name)
			}
			values[i] = n
		}
	}
	if err := validResources(values[0], values[1]); err != nil {
		return 0, 0, err
	}
	return values[0], values[1], nil
}

func vmResources() (int, int, error) { return vmResourcesFor("") }

// growDataDisk extends the sparse data disk to the size an administrator approved.
// It only ever grows the file; the guest enlarges the filesystem when it boots. If
// the computer has less free space than the growth, it refuses rather than let the
// disk fill the host.
func growDataDisk(state string) error {
	a, ok := readApprovedResources(state)
	if !ok || a.DiskGiB == 0 {
		return nil
	}
	path := filepath.Join(state, "data.img")
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	want := int64(a.DiskGiB) << 30
	if want <= info.Size() {
		return nil
	}
	if err := diskGrowthRefusal(state, want, info.Size()); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(want); err != nil {
		return err
	}
	return f.Sync()
}

// freeFloor is the free space this computer keeps after the data disk grows: the larger of
// 20 GiB and a tenth of the volume. The disk is sparse, so growth costs nothing until the
// cluster writes, but a full host is a worse failure than a refused request.
func freeFloor(total uint64) int64 {
	return max(int64(20)<<30, int64(total/10))
}

// diskGrowthRefusal says why the data disk may not grow from current to want bytes, or nil.
func diskGrowthRefusal(state string, want, current int64) error {
	if want > maxDataDiskBytes {
		return fmt.Errorf("the data disk may not exceed %d GiB", maxDataDiskBytes>>30)
	}
	u, err := diskspace.Of(state)
	if err != nil {
		return nil // cannot tell; the existing behaviour is to proceed
	}
	growth, floor := want-current, freeFloor(u.Total)
	if growth > 0 && int64(u.Free)-growth < floor {
		return fmt.Errorf("not growing the data disk to %d GiB: this computer has %d GiB free and must keep at least %d GiB free after the growth", want>>30, u.Free>>30, floor>>30)
	}
	return nil
}

// dataDiskFact reports the disk's size, real cost and how much it could still grow.
func dataDiskFact(state string) *hostapi.DataDisk {
	info, err := os.Stat(filepath.Join(state, "data.img"))
	if err != nil {
		return nil
	}
	alloc, _ := allocatedBytes(filepath.Join(state, "data.img"))
	d := &hostapi.DataDisk{SizeBytes: info.Size(), AllocatedBytes: alloc, CeilingBytes: maxDataDiskBytes}
	if u, err := diskspace.Of(state); err == nil {
		d.FreeFloorBytes = freeFloor(u.Total)
		d.MaxGrowthBytes = max(0, min(int64(maxDataDiskBytes)-info.Size(), int64(u.Free)-d.FreeFloorBytes))
	}
	return d
}
