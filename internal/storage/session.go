package storage

import (
	"os"
	"path/filepath"
	"time"
)

const (
	sessionFile = "containers-anywhere/session"
	imageCache  = "rancher/k3s/agent/containerd"
)

// BeginSession records that the appliance is running and reports whether the
// previous session ended without a clean shutdown (a panic, a power loss, a killed
// VM). After such an end, files that containerd extracted from image layers can be
// empty even though containerd believes the layers are committed: btrfs keeps the
// file but not the data that had not been written yet. The image cache is only a
// copy of what registries hold, so the caller discards it rather than trust it.
func BeginSession(varLib string) (unclean bool, err error) {
	path := filepath.Join(varLib, sessionFile)
	if _, statErr := os.Lstat(path); statErr == nil {
		unclean = true
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return unclean, err
	}
	return unclean, os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

// EndSession marks a clean shutdown. It runs after the cluster has stopped.
func EndSession(varLib string) error {
	err := os.Remove(filepath.Join(varLib, sessionFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ResetImageCache empties the container image cache, keeping the directory (a
// subvolume) in place. Workload volumes and the cluster database are elsewhere
// and are never touched.
func ResetImageCache(varLib string) error {
	dir := filepath.Join(varLib, imageCache)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	var first error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && first == nil {
			first = err
		}
	}
	return first
}
