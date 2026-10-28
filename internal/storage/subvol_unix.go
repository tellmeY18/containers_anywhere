//go:build darwin || linux

package storage

import (
	"os"
	"syscall"
)

// isSubvolumeRoot reports a btrfs subvolume's root directory: its inode is always 256. It lets a
// tree walk find nested subvolumes without starting a process for every file.
func isSubvolumeRoot(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && st.Ino == 256
}
