package backuprepo

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File) (func(), error) {
	o := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, o); err != nil {
		f.Close()
		return nil, errors.Join(ErrLocked, err)
	}
	return func() { windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, o); f.Close() }, nil
}

// Windows has no os.File directory fsync. Files are flushed before publication;
// NTFS power-loss ordering still requires platform acceptance.
func syncRoot(*os.Root) error { return nil }

// CARE Clinic private ACL pattern, also used by internal/atomicfile.
// Copyright (c) 2026 Open Healthcare Network Foundation. MIT; see
// THIRD_PARTY_NOTICES.md. Child files inherit only the current owner's access.
func securityDescriptor(inherit bool) (*windows.SECURITY_DESCRIPTOR, error) {
	t, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer t.Close()
	u, err := t.GetTokenUser()
	if err != nil {
		return nil, err
	}
	flags := ""
	if inherit {
		flags = "OICI"
	}
	return windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;" + u.User.Sid.String() + ")")
}

func privateDirectory(path string) error {
	d, err := securityDescriptor(true)
	if err != nil {
		return err
	}
	acl, _, err := d.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// Set the protected owner-only DACL in CREATE_NEW itself. Creating an inherited
// ACL and narrowing it afterward allows an observer to keep a readable handle.
func createPrivateFile(root *os.Root, name string) (*os.File, error) {
	if filepath.Base(name) != name || !filepath.IsLocal(name) {
		return nil, errors.New("private filename must be a safe single component")
	}
	if err := sameDirectory(root, root.Name()); err != nil {
		return nil, err
	}
	d, err := securityDescriptor(false)
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: d}
	full := filepath.Join(root.Name(), name)
	path, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, &sa, windows.CREATE_NEW, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(d)
	if err != nil {
		return nil, &os.PathError{Op: "create-private", Path: full, Err: err}
	}
	f := os.NewFile(uintptr(h), full)
	if f == nil {
		windows.CloseHandle(h)
		return nil, errors.New("invalid private file handle")
	}
	// Native CreateFile needs a pathname. Before writing any secret, prove that
	// its handle still names the exclusive file inside the anchored directory.
	if err := sameDirectory(root, root.Name()); err != nil {
		f.Close()
		return nil, err
	}
	created, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	anchored, err := root.Lstat(name)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !anchored.Mode().IsRegular() || !os.SameFile(created, anchored) {
		f.Close()
		return nil, errors.New("private publication file was replaced")
	}
	return f, nil
}
