//go:build linux

package main

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

func hostMemoryMiB() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if fields := strings.Fields(s.Text()); len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			return kb >> 10, err
		}
	}
	return 0, errors.New("MemTotal not found")
}
