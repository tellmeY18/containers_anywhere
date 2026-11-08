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
	name := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return string(name) == "exfat" || string(name) == "msdos"
}
