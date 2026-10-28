package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var safeVersion = regexp.MustCompile(`[^A-Za-z0-9.-]`)

type bootRecord struct {
	Version  string `json:"version"`
	BootedAt string `json:"bootedAt"`
}

type restorePointMeta struct {
	Label string `json:"label,omitempty"`
	From  string `json:"from"`
	// K3s is the Kubernetes minor the point's cluster ran. A restore is only safe under the same
	// minor, because K3s never moves its datastore backwards between minors.
	K3s       string   `json:"k3s,omitempty"`
	To        string   `json:"to"`
	CreatedAt string   `json:"createdAt"`
	Volumes   []string `json:"volumes"`
}

// RestorePointConfig describes where state lives and how many restore points to keep. PoolDir
// is the btrfs pool the CSI driver manages: its volumes are subvolumes under PoolDir/volumes,
// and its state.json maps their ids to claims.
type RestorePointConfig struct {
	VarLib, SnapshotsDir, PoolDir string
	Version                       string
	K3s                           string // the Kubernetes major.minor this appliance runs
	Keep                          int
	// Protect names a restore point that pruning must not remove (the one being restored from).
	Protect string
	Now     func() time.Time
}

// TakeRestorePointOnChange snapshots the system subvolume and every volume the first
// time a different appliance version boots on this disk, before the cluster starts
// and writes anything. It is a crash-consistent copy of the state the previous
// root left behind, kept on the same disk: it protects against a bad update, not
// against losing the disk. It never blocks boot; the caller logs any error.
func TakeRestorePointOnChange(ctx context.Context, fs FS, cfg RestorePointConfig) (string, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Keep < 1 {
		cfg.Keep = 2
	}
	path := filepath.Join(cfg.VarLib, "containers-anywhere", "boot.json")
	var prev bootRecord
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &prev)
	}
	if prev.Version == cfg.Version {
		return "", nil
	}
	var made string
	var snapErr error
	if prev.Version != "" {
		made, snapErr = takeRestorePoint(ctx, fs, cfg, prev.Version)
	}
	// Record the version even if the snapshot failed, so a persistent failure
	// cannot stall every boot.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return made, err
	}
	b, _ := json.Marshal(bootRecord{Version: cfg.Version, BootedAt: cfg.Now().UTC().Format(time.RFC3339)})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return made, err
	}
	return made, snapErr
}

func takeRestorePoint(ctx context.Context, fs FS, cfg RestorePointConfig, from string) (string, error) {
	return takeLabelledRestorePoint(ctx, fs, cfg, from, from)
}

// takeLabelledRestorePoint records the state left by version "from", named after label.
func takeLabelledRestorePoint(ctx context.Context, fs FS, cfg RestorePointConfig, from, label string) (string, error) {
	if _, err := os.Stat(cfg.SnapshotsDir); err != nil {
		return "", fmt.Errorf("snapshots are not available: %w", err)
	}
	stamp := cfg.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(cfg.SnapshotsDir, "restore-"+stamp+"-from-"+safeVersion.ReplaceAllString(label, "_"))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	meta := restorePointMeta{Label: label, From: from, K3s: cfg.K3s, To: cfg.Version, CreatedAt: cfg.Now().UTC().Format(time.RFC3339)}
	if err := fs.Snapshot(ctx, cfg.VarLib, filepath.Join(dir, "system"), true); err != nil {
		return dir, err
	}
	vols, _ := os.ReadDir(filepath.Join(cfg.PoolDir, "volumes"))
	for _, v := range vols {
		src := filepath.Join(cfg.PoolDir, "volumes", v.Name())
		if !fs.IsSubvolume(ctx, src) {
			continue
		}
		if err := fs.Snapshot(ctx, src, filepath.Join(dir, "vol-"+v.Name()), true); err != nil {
			return dir, err
		}
		meta.Volumes = append(meta.Volumes, v.Name())
	}
	// The driver's state file says which volume id belongs to which claim.
	if b, err := os.ReadFile(filepath.Join(cfg.PoolDir, "state.json")); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "pool-state.json"), b, 0o600)
	}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o600); err != nil {
		return dir, err
	}
	return dir, pruneRestorePoints(ctx, fs, cfg)
}

func pruneRestorePoints(ctx context.Context, fs FS, cfg RestorePointConfig) error {
	entries, err := os.ReadDir(cfg.SnapshotsDir)
	if err != nil {
		return err
	}
	var points []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "restore-") {
			points = append(points, e.Name())
		}
	}
	sort.Strings(points) // the timestamp makes name order chronological
	var firstErr error
	for i := 0; i < len(points)-cfg.Keep; i++ {
		if points[i] == cfg.Protect {
			continue
		}
		dir := filepath.Join(cfg.SnapshotsDir, points[i])
		subs, _ := os.ReadDir(dir)
		for _, s := range subs {
			if p := filepath.Join(dir, s.Name()); fs.IsSubvolume(ctx, p) {
				if err := fs.DeleteSubvolume(ctx, p); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		if err := os.RemoveAll(dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
