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
	"strconv"
	"strings"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/protocol"
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
	K3s       string   `json:"k3s,omitempty"`
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

// parseK3s turns "1.35.8+k3s1" into comparable numbers. ok is false when the string does not
// name at least a major and a minor version.
func parseK3s(v string) (parts [3]int, build int, ok bool) {
	core, buildPart := strings.TrimPrefix(strings.TrimSpace(v), "v"), ""
	if i := strings.Index(core, "+"); i >= 0 {
		core, buildPart = core[:i], core[i+1:]
	}
	fields := strings.Split(core, ".")
	if len(fields) < 2 {
		return parts, 0, false
	}
	for i := 0; i < len(fields) && i < len(parts); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			return parts, 0, false
		}
		parts[i] = n
	}
	if i := strings.LastIndexFunc(buildPart, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		build, _ = strconv.Atoi(buildPart[i+1:])
	} else {
		build, _ = strconv.Atoi(buildPart)
	}
	return parts, build, true
}

// k3sOrder is negative when a is older than b.
func k3sOrder(a [3]int, ab int, b [3]int, bb int) int {
	for i := range a {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return ab - bb
}

// contractOf reports the data contract a restore point's state declares.
func contractOf(systemDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(systemDir, "containers-anywhere", "data-format.json"))
	if err != nil {
		return "", err
	}
	var f struct {
		Contract string `json:"contract"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return "", err
	}
	return f.Contract, nil
}

// restorable says whether this appliance can put a point's state back, and why not when it
// cannot. None of this is about appliance versions: a restore replaces the disk, and whatever
// appliance boots next reads that disk, so the only question is whether the state and the running
// appliance form a supported pair.
//
// The hard constraint is K3s, not Containers Anywhere: a K3s datastore is migrated forward on
// start and never backwards, so state written by a newer K3s, or by another minor, cannot be
// opened by the K3s in this appliance. That is the same one-way rule the launcher applies to
// updates. The other half is the data contract, which the appliance checks before anything writes
// to /var/lib.
func restorable(systemDir string, m restorePointMeta, currentVersion, currentK3s string) (bool, string) {
	if m.From == currentVersion {
		return true, ""
	}
	if m.K3s == "" {
		return false, "this point does not record the Kubernetes version that wrote its state (" + m.From + "), so it cannot be checked for compatibility"
	}
	point, pb, pointOK := parseK3s(m.K3s)
	now, nb, nowOK := parseK3s(currentK3s)
	switch {
	case !nowOK:
		return false, "this appliance does not report its Kubernetes version, so the point cannot be checked"
	case !pointOK:
		return false, "the point records an unreadable Kubernetes version (" + m.K3s + ")"
	case point[0] != now[0] || point[1] != now[1]:
		return false, fmt.Sprintf("this point's state was written by Kubernetes %s and this appliance runs %s; K3s cannot open a datastore from another minor, so %s has to run to restore it, or take a point under this appliance first", m.K3s, currentK3s, m.From)
	case k3sOrder(point, pb, now, nb) > 0:
		return false, fmt.Sprintf("this point's state was written by Kubernetes %s, newer than this appliance's %s; K3s cannot step a datastore backwards, so %s has to run to restore it", m.K3s, currentK3s, m.From)
	}
	contract, err := contractOf(systemDir)
	if err != nil {
		return false, "the point's state has no readable data format marker, so this appliance cannot vouch for it"
	}
	if contract != protocol.DataContract {
		return false, fmt.Sprintf("the point's state uses data format %q but this appliance supports %q", contract, protocol.DataContract)
	}
	return true, ""
}

// ListRestorePoints returns the points on this disk, newest first.
func ListRestorePoints(snapshotsDir, currentVersion, currentK3s string) ([]RestorePoint, error) {
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
		p := RestorePoint{ID: e.Name(), From: m.From, To: m.To, Label: m.Label, CreatedAt: m.CreatedAt, Volumes: m.Volumes, K3s: m.K3s}
		if err != nil {
			p.Reason = "the restore point has no readable description"
		} else {
			p.Restorable, p.Reason = restorable(filepath.Join(snapshotsDir, e.Name(), "system"), m, currentVersion, currentK3s)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// RequestRestore records that the next boot must restore the given point. It changes
// nothing else: the restore happens before the cluster starts, in ApplyPendingRestore.
func RequestRestore(snapshotsDir, id, currentVersion, currentK3s string, now time.Time) error {
	if !pointName.MatchString(id) {
		return errors.New("not a restore point")
	}
	points, err := ListRestorePoints(snapshotsDir, currentVersion, currentK3s)
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
	if ok, why := restorable(filepath.Join(target, "system"), meta, cfg.Version, cfg.K3s); !ok {
		os.Remove(path)
		return finish(errors.New(why + "; nothing was changed"))
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

// TakeManualRestorePoint snapshots the current state on request, so a person can keep a way back
// before a risky change without waiting for an appliance update. Like the automatic ones it is a
// crash-consistent copy of a running cluster, kept on the same disk.
func TakeManualRestorePoint(ctx context.Context, fs FS, cfg RestorePointConfig) (string, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Keep < 1 {
		cfg.Keep = 2
	}
	if _, err := os.Stat(cfg.SnapshotsDir); err != nil {
		return "", fmt.Errorf("snapshots are not available: %w", err)
	}
	return takeLabelledRestorePoint(ctx, fs, cfg, cfg.Version, "manual")
}
