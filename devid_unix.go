//go:build darwin || linux

package main

import (
	"fmt"
	"syscall"
)

// deviceID identifies the filesystem a path lives on.
func deviceID(path string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("dev:%d", st.Dev), nil
}
