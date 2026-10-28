package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrStateInconsistent means a restore failed and the previous state could not be put back: the
// cluster must not start on what is on the disk. The restore points still hold both states.
var ErrStateInconsistent = errors.New("the data disk is in an inconsistent state")

const (
	pendingRestoreFile = "pending-restore.json"
	lastRestoreFile    = "last-restore.json"
	poolVolumes        = "volumes"
)

var pointName = regexp.MustCompile(`^restore-[0-9]{8}T[0-9]{6}Z-from-[A-Za-z0-9._-]+$`)

// Copier copies the contents of directory src into the existing directory dst, preserving
// ownership, modes, links and extended attributes (on btrfs it reflinks, so it is cheap).
type Copier func(ctx context.Context, src, dst string) error

// RestoreConfig describes a restore of the whole appliance state from a restore point.
type RestoreConfig struct {
	RestorePointConfig
	// Copy copies a point's @system snapshot over /var/lib.
	Copy Copier
}

// RestorePoint is one restore point as listed for a person to choose from.
type RestorePoint struct {
	ID        string   `json:"id"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	Label     string   `json:"label,omitempty"`
	CreatedAt string   `json:"createdAt"`
	Volumes   []string `json:"volumes"`
	// Restorable says whether the appliance now running can restore it.
	Restorable bool   `json:"restorable"`
	Reason     string `json:"reason,omitempty"`
}

type pendingRestore struct {
	ID          string `json:"id"`
	Phase       string `json:"phase"` // "requested", then "restoring"
	RequestedAt string `json:"requestedAt"`
	Safety      string `json:"safety,omitempty"` // the restore point taken of the state being replaced
}

// RestoreResult is what the last restore attempt did, kept for the launcher to report.
type RestoreResult struct {
	ID          string `json:"id"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	Safety      string `json:"safety,omitempty"`
	RolledBack  bool   `json:"rolledBack,omitempty"`
	CompletedAt string `json:"completedAt"`
}

func readMeta(dir string) (restorePointMeta, error) {
	var m restorePointMeta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// ListRestorePoints returns the points on this disk, newest first.
func ListRestorePoints(snapshotsDir, currentVersion string) ([]RestorePoint, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		return nil, err
	}
	var out []RestorePoint
	for _, e := range entries {
		if !e.IsDir() || !pointName.MatchString(e.Name()) {
			continue
		}
		m, err := readMeta(filepath.Join(snapshotsDir, e.Name()))
		p := RestorePoint{ID: e.Name(), From: m.From, To: m.To, Label: m.Label, CreatedAt: m.CreatedAt, Volumes: m.Volumes}
		switch {
		case err != nil:
			p.Reason = "the restore point has no readable description"
		case m.From != currentVersion:
			p.Reason = fmt.Sprintf("this point was taken under appliance %s and %s is running; start the matching appliance first", m.From, currentVersion)
		default:
			p.Restorable = true
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// RequestRestore records that the next boot must restore the given point. It changes
// nothing else: the restore happens before the cluster starts, in ApplyPendingRestore.
func RequestRestore(snapshotsDir, id, currentVersion string, now time.Time) error {
	if !pointName.MatchString(id) {
		return errors.New("not a restore point")
	}
	points, err := ListRestorePoints(snapshotsDir, currentVersion)
	if err != nil {
		return err
	}
	for _, p := range points {
		if p.ID == id {
			if !p.Restorable {
				return errors.New(p.Reason)
			}
			if _, err := os.Stat(filepath.Join(snapshotsDir, pendingRestoreFile)); err == nil {
				return errors.New("a restore is already waiting for the next start")
			}
			return writeJSON(filepath.Join(snapshotsDir, pendingRestoreFile), pendingRestore{ID: id, Phase: "requested", RequestedAt: now.UTC().Format(time.RFC3339)})
		}
	}
	return errors.New("unknown restore point")
}

// PendingRestore reports a restore waiting for the next start, or "" if none.
func PendingRestore(snapshotsDir string) string {
	var p pendingRestore
	if b, err := os.ReadFile(filepath.Join(snapshotsDir, pendingRestoreFile)); err == nil && json.Unmarshal(b, &p) == nil {
		return p.ID
	}
	return ""
}

// LastRestore reports the most recent restore attempt.
func LastRestore(snapshotsDir string) (RestoreResult, bool) {
	var r RestoreResult
	b, err := os.ReadFile(filepath.Join(snapshotsDir, lastRestoreFile))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return r, false
	}
	return r, true
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ApplyPendingRestore performs a requested restore. It runs before the cluster starts, while
// nothing writes to /var/lib. It reports whether it restored, and an error only when the
// state could not be made consistent (the caller must then refuse to start the cluster).
//
// The state being replaced is first kept as a restore point of its own. If the restore fails
// part-way, that point is restored in its place; if a crash interrupts it, the next boot
// resumes it. Nothing is ever deleted without that point existing first.
func ApplyPendingRestore(ctx context.Context, fs FS, cfg RestoreConfig) (bool, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	path := filepath.Join(cfg.SnapshotsDir, pendingRestoreFile)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	var pend pendingRestore
	if err != nil || json.Unmarshal(b, &pend) != nil || !pointName.MatchString(pend.ID) {
		os.Remove(path)
		return false, errors.New("ignoring an unreadable restore request")
	}
	result := RestoreResult{ID: pend.ID}
	finish := func(err error) (bool, error) {
		result.CompletedAt = cfg.Now().UTC().Format(time.RFC3339)
		result.OK = err == nil
		if err != nil {
			result.Error = err.Error()
		}
		_ = writeJSON(filepath.Join(cfg.SnapshotsDir, lastRestoreFile), result)
		return err == nil, err
	}
	target := filepath.Join(cfg.SnapshotsDir, pend.ID)
	meta, err := readMeta(target)
	if err != nil || !fs.IsSubvolume(ctx, filepath.Join(target, "system")) {
		os.Remove(path)
		return finish(errors.New("the restore point is missing or damaged; nothing was changed"))
	}
	if meta.From != cfg.Version {
		os.Remove(path)
		return finish(fmt.Errorf("the restore point was taken under appliance %s and %s is running; nothing was changed", meta.From, cfg.Version))
	}
	if pend.Phase != "restoring" {
		cfg.Protect = pend.ID
		safety, err := takeLabelledRestorePoint(ctx, fs, cfg.RestorePointConfig, cfg.Version, "before-restore")
		if err != nil {
			os.Remove(path)
			return finish(fmt.Errorf("could not keep the current state first, so nothing was changed: %w", err))
		}
		pend.Phase, pend.Safety = "restoring", filepath.Base(safety)
		if err := writeJSON(path, pend); err != nil {
			return finish(fmt.Errorf("could not record the restore, so nothing was changed: %w", err))
		}
	}
	result.Safety = pend.Safety
	if err := restoreFrom(ctx, fs, cfg, target, meta); err != nil {
		// Put back what was there before.
		safetyDir := filepath.Join(cfg.SnapshotsDir, pend.Safety)
		sm, serr := readMeta(safetyDir)
		if pend.Safety == "" || serr != nil {
			return finish(fmt.Errorf("%w: the restore failed (%v) and the state kept before it could not be read; the cluster will not start", ErrStateInconsistent, err))
		}
		if rerr := restoreFrom(ctx, fs, cfg, safetyDir, sm); rerr != nil {
			return finish(fmt.Errorf("%w: the restore failed (%v) and putting the previous state back failed too (%v); the cluster will not start. The previous state is the restore point %s", ErrStateInconsistent, err, rerr, pend.Safety))
		}
		result.RolledBack = true
		os.Remove(path)
		return finish(fmt.Errorf("the restore failed and the previous state was put back: %w", err))
	}
	os.Remove(path)
	return finish(nil)
}

// restoreFrom replaces /var/lib's contents and the pool's volumes with those of a restore point.
// It keeps the container image cache, the journal and the pool itself (those are subvolumes in
// use or re-creatable), empties the image cache, and leaves CSI snapshots alone.
func restoreFrom(ctx context.Context, fs FS, cfg RestoreConfig, point string, meta restorePointMeta) error {
	system := filepath.Join(point, "system")
	if !fs.IsSubvolume(ctx, system) {
		return errors.New("the restore point has no system snapshot")
	}
	keep := map[string]bool{
		filepath.Join(cfg.VarLib, imageCache):                       true,
		filepath.Join(cfg.VarLib, "containers-anywhere", "journal"): true,
		cfg.PoolDir: true,
	}
	if err := wipeExcept(ctx, fs, cfg.VarLib, keep); err != nil {
		return fmt.Errorf("clearing the current state: %w", err)
	}
	if cfg.Copy == nil {
		return errors.New("no copier configured")
	}
	if err := cfg.Copy(ctx, system, cfg.VarLib); err != nil {
		return fmt.Errorf("copying the restore point's system state: %w", err)
	}
	if err := restorePool(ctx, fs, cfg, point, meta); err != nil {
		return fmt.Errorf("restoring volumes: %w", err)
	}
	if err := ResetImageCache(cfg.VarLib); err != nil {
		return fmt.Errorf("emptying the image cache: %w", err)
	}
	return nil
}

func restorePool(ctx context.Context, fs FS, cfg RestoreConfig, point string, meta restorePointMeta) error {
	vols := filepath.Join(cfg.PoolDir, poolVolumes)
	if err := os.MkdirAll(vols, 0o755); err != nil {
		return err
	}
	current, _ := os.ReadDir(vols)
	for _, v := range current {
		p := filepath.Join(vols, v.Name())
		if fs.IsSubvolume(ctx, p) {
			if err := fs.DeleteSubvolume(ctx, p); err != nil {
				return err
			}
		} else if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	for _, id := range meta.Volumes {
		if !safeID.MatchString(id) {
			return fmt.Errorf("invalid volume id %q in the restore point", id)
		}
		if err := fs.Snapshot(ctx, filepath.Join(point, "vol-"+id), filepath.Join(vols, id), false); err != nil {
			return fmt.Errorf("volume %s: %w", id, err)
		}
	}
	state := filepath.Join(cfg.PoolDir, "state.json")
	if b, err := os.ReadFile(filepath.Join(point, "pool-state.json")); err == nil {
		return os.WriteFile(state, b, 0o600)
	}
	if err := os.Remove(state); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// wipeExcept removes everything under root except the kept paths (and the directories that lead to them).
func wipeExcept(ctx context.Context, fs FS, root string, keep map[string]bool) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if keep[p] {
			continue
		}
		leadsToKept := false
		for k := range keep {
			if strings.HasPrefix(k, p+string(filepath.Separator)) {
				leadsToKept = true
			}
		}
		if leadsToKept {
			if err := wipeExcept(ctx, fs, p, keep); err != nil {
				return err
			}
			continue
		}
		if fs.IsSubvolume(ctx, p) {
			if err := fs.DeleteSubvolume(ctx, p); err != nil {
				return err
			}
			continue
		}
		if err := removeAllSubvolumeAware(ctx, fs, p); err != nil {
			return err
		}
	}
	return nil
}

// removeAllSubvolumeAware deletes a tree that may contain nested subvolumes (a plain RemoveAll
// cannot delete those).
func removeAllSubvolumeAware(ctx context.Context, fs FS, p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if isSubvolumeRoot(info) {
		return fs.DeleteSubvolume(ctx, p)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := removeAllSubvolumeAware(ctx, fs, filepath.Join(p, e.Name())); err != nil {
				return err
			}
		}
	}
	return os.Remove(p)
}
