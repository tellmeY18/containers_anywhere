//go:build !windows

package storage

import (
	"os"
	"syscall"
)

// EnsureRootOwned makes each directory root-owned with mode 0755. The data disk
// template is built unprivileged, so the roots of its subvolumes can carry the
// builder's uid; systemd-tmpfiles then refuses to create anything beneath a
// directory owned by another user ("unsafe path transition").
func EnsureRootOwned(paths ...string) error {
	var first error
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			if !os.IsNotExist(err) && first == nil {
				first = err
			}
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid == 0 && st.Gid == 0 && info.Mode().Perm() == 0o755 {
			continue
		}
		if err := os.Chown(p, 0, 0); err != nil && first == nil {
			first = err
		}
		if err := os.Chmod(p, 0o755); err != nil && first == nil {
			first = err
		}
	}
	return first
}
