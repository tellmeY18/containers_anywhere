package main

import (
	"fmt"
	"os"
	"strconv"
)

func vmResources() (int, int, error) {
	values := []int{2, 4096}
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
