//go:build darwin

package main

import "golang.org/x/sys/unix"

func hostMemoryMiB() (int64, error) {
	b, err := unix.SysctlUint64("hw.memsize")
	return int64(b >> 20), err
}
