package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const approvedResourcesFile = "resources.json"

// approvedResources is a size an administrator approved in the launcher. It takes
// effect only when the appliance next starts, and never touches the data disk.
type approvedResources struct {
	CPUs       int    `json:"cpus"`
	MemoryMiB  int    `json:"memoryMiB"`
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
