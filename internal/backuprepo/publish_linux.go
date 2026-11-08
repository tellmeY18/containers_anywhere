package backuprepo

import (
	"errors"
	"os"
	"syscall"
)

func linkUnsupported(root *os.Root, err error) bool {
	if errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) {
		return true
	}
	// Linux VFS returns EPERM for filesystems without an inode link operation.
	// Do not interpret arbitrary permission errors as unsupported: inspect the
	// anchored filesystem, and recognize only FAT/exFAT here.
	if !errors.Is(err, syscall.EPERM) {
		return false
	}
	f, e := root.Open(".")
	if e != nil {
		return false
	}
	defer f.Close()
	var st syscall.Statfs_t
	if syscall.Fstatfs(int(f.Fd()), &st) != nil {
		return false
	}
	return uint64(st.Type) == 0x4d44 || uint64(st.Type) == 0x2011bab0
}
