//go:build linux || darwin

package recovery

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// flock also serializes distinct Engine instances and the boot/serving processes.
func (e *Engine) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(e.cfg.Root, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}
