//go:build darwin || linux

package backuprepo

import (
	"errors"
	"os"
	"syscall"
)

func lockFile(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.Join(ErrLocked, err)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func syncRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func privateDirectory(path string) error {
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	s, err := os.Stat(path)
	if err != nil {
		return err
	}
	if s.Mode().Perm()&0077 != 0 {
		return errors.New("staging filesystem cannot enforce private directory permissions")
	}
	return nil
}

func privateFile(f *os.File) error {
	if err := f.Chmod(0600); err != nil {
		return err
	}
	s, err := f.Stat()
	if err != nil {
		return err
	}
	if s.Mode().Perm()&0077 != 0 {
		return errors.New("key filesystem cannot enforce private file permissions")
	}
	return nil
}

func createPrivateFile(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := privateFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
