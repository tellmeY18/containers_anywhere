// Package storage manages the btrfs data disk: per-volume subvolumes for
// PersistentVolumeClaims, workload snapshots, and the restore point taken when the
// appliance root changes. Nothing here is specific to an application.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FS is the small set of btrfs operations the controller needs. It exists so the
// controller can be tested without a btrfs filesystem.
type FS interface {
	CreateSubvolume(ctx context.Context, path string) error
	Snapshot(ctx context.Context, src, dst string, readOnly bool) error
	DeleteSubvolume(ctx context.Context, path string) error
	IsSubvolume(ctx context.Context, path string) bool
	SetNoCOW(ctx context.Context, path string) error
	FreeFraction(path string) (float64, error)
}

type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// BtrfsFS drives the btrfs and chattr command line tools.
type BtrfsFS struct{ run runner }

func NewBtrfsFS() *BtrfsFS { return &BtrfsFS{run: execRunner} }

func (f *BtrfsFS) CreateSubvolume(ctx context.Context, path string) error {
	_, err := f.run(ctx, "btrfs", "subvolume", "create", path)
	return err
}

func (f *BtrfsFS) Snapshot(ctx context.Context, src, dst string, readOnly bool) error {
	args := []string{"subvolume", "snapshot"}
	if readOnly {
		args = append(args, "-r")
	}
	_, err := f.run(ctx, "btrfs", append(args, src, dst)...)
	return err
}

func (f *BtrfsFS) DeleteSubvolume(ctx context.Context, path string) error {
	_, err := f.run(ctx, "btrfs", "subvolume", "delete", path)
	return err
}

func (f *BtrfsFS) IsSubvolume(ctx context.Context, path string) bool {
	_, err := f.run(ctx, "btrfs", "subvolume", "show", path)
	return err == nil
}

// SetNoCOW turns copy-on-write off for files created afterwards in an empty
// directory. It trades btrfs checksums and cheap snapshots of that data for
// steadier random writes, so it is only used when a volume class asks for it.
func (f *BtrfsFS) SetNoCOW(ctx context.Context, path string) error {
	_, err := f.run(ctx, "chattr", "+C", path)
	return err
}

func (f *BtrfsFS) FreeFraction(path string) (float64, error) { return freeFraction(path) }

// within reports whether p is strictly inside dir after cleaning, so a crafted
// name can never point the controller outside the directories it owns.
func within(dir, p string) bool {
	dir, p = filepath.Clean(dir), filepath.Clean(p)
	return p != dir && strings.HasPrefix(p, dir+string(filepath.Separator))
}

// EnsureLayout creates the nested subvolumes that keep re-downloadable or churny
// data out of restore points. It never converts or removes an existing directory.
func EnsureLayout(ctx context.Context, fs FS, varLib string) error {
	var errs []error
	for _, rel := range []string{
		"rancher/k3s/agent/containerd",
		"rancher/k3s/storage",
		"containers-anywhere/journal",
	} {
		p := filepath.Join(varLib, rel)
		if _, err := os.Lstat(p); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := fs.CreateSubvolume(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
