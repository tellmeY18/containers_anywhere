package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func vmResources() (int, int, error) {
	values := []int{2, defaultMemoryMiB()}
	for i, name := range []string{"CONTAINERS_CPUS", "CONTAINERS_MEMORY_MIB"} {
		if s := os.Getenv(name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				return 0, 0, fmt.Errorf("%s must be an integer", name)
			}
			values[i] = n
		}
	}
	if values[0] < 2 || values[0] > 64 || values[1] < 2048 || values[1] > 262144 {
		return 0, 0, fmt.Errorf("VM requires 2–64 CPUs and 2048–262144 MiB RAM")
	}
	return values[0], values[1], nil
}

// defaultMemoryMiB gives the guest 8 GiB when the host has at least 16 GiB,
// since CARE image builds do not fit comfortably in 4 GiB. Detection failures
// keep the conservative 4 GiB default.
func defaultMemoryMiB() int {
	var total uint64
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			total, _ = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
			total >>= 20
		}
	case "linux":
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			var kib uint64
			if _, err = fmt.Sscanf(string(b), "MemTotal: %d kB", &kib); err == nil {
				total = kib >> 10
			}
		}
	}
	if total >= 15*1024 {
		return 8192
	}
	return 4096
}
