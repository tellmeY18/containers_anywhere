//go:build !darwin && !linux && !windows

package main

import "errors"

func hostMemoryMiB() (int64, error) {
	return 0, errors.New("host memory is not available on this platform")
}
