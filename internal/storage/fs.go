// Package storage holds the appliance's boot-time duties for the btrfs data disk: a root-owned
// mount root, the nested subvolumes for the image cache and journal, the session marker that
// detects an unclean shutdown, and the restore point taken when the appliance version changes.
// Volumes, snapshots and clones are not here: they are a standard CSI driver run by the app.
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

// EnsureLayout creates the nested subvolumes that keep re-downloadable or churny
// data out of restore points. It never converts or removes an existing directory.
func EnsureLayout(ctx context.Context, fs FS, varLib string) error {
	var errs []error
	for _, rel := range []string{
		"rancher/k3s/agent/containerd",
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
