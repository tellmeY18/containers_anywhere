package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// ReplaceSystem is a cold-only primitive. The caller must durably preserve a
// safety point and journal before calling it; it deliberately does not own those.
func ReplaceSystem(ctx context.Context, fs FS, varLib, pool, system string, copy Copier) error {
	if copy == nil {
		return errors.New("no system copier")
	}
	keep := map[string]bool{filepath.Join(varLib, imageCache): true, filepath.Join(varLib, "containers-anywhere/journal"): true, pool: true}
	if err := wipeExcept(ctx, fs, varLib, keep); err != nil {
		return err
	}
	return copy(ctx, system, varLib)
}

// RemoveTree removes descendants before their parent subvolume. It never follows
// symlinks. Btrfs does not recursively delete nested subvolumes itself.
func RemoveTree(ctx context.Context, fs FS, path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.IsDir() && fs.IsSubvolume(ctx, path) {
		if err := removeNested(ctx, fs, path); err != nil {
			return err
		}
		return fs.DeleteSubvolume(ctx, path)
	}
	if st.IsDir() {
		es, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range es {
			if err := RemoveTree(ctx, fs, filepath.Join(path, e.Name())); err != nil {
				return err
			}
		}
		if fs.IsSubvolume(ctx, path) {
			return fs.DeleteSubvolume(ctx, path)
		}
	}
	return os.Remove(path)
}

func removeNested(ctx context.Context, fs FS, path string) error {
	es, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range es {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(path, e.Name())
		if fs.IsSubvolume(ctx, p) {
			if err := RemoveTree(ctx, fs, p); err != nil {
				return err
			}
		} else if err := removeNested(ctx, fs, p); err != nil {
			return err
		}
	}
	return nil
}
