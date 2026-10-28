//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// deviceID identifies the volume a path lives on (its drive or UNC share).
func deviceID(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return "vol:" + strings.ToLower(filepath.VolumeName(abs)), nil
}

func allocatedBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
