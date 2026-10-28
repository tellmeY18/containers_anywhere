//go:build windows

package main

import (
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
