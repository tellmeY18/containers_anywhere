//go:build !windows

package storage

import "syscall"

func freeFraction(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks == 0 {
		return 0, nil
	}
	return float64(st.Bavail) / float64(st.Blocks), nil
}
