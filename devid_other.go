//go:build !darwin && !linux && !windows

package main

import (
	"errors"
	"os"
)

func deviceID(string) (string, error) { return "", errors.New("not supported on this platform") }

func allocatedBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
