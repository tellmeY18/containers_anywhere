package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
	"github.com/tellmeY18/containers_anywhere/internal/hostapi"
)

const (
	recoveryKeyFile    = "recovery-key.txt"
	keyExportedFile    = "recovery-key-exported.json"
	recoveryPointsFile = "recovery-points.json"
	recoveryJobFile    = "recovery-job.json"
	scheduleFile       = "backup-schedule.json"
	maxStoredPoints    = 200
)

// recoveryMu serialises the small state files below. The launcher is one process,
// and the cold backup runs between two appliance sessions in that same process.
var recoveryMu sync.Mutex

type recoveryJob struct {
	ID          string `json:"id"`
	Destination string `json:"destination"`
	QueuedAt    string `json:"queuedAt"`
}

// recoveryIdentity is the key every recovery point is encrypted to. It lives in the
// launcher's state directory, outside the cluster, and a destination only ever holds
// ciphertext. It stays on this computer until the person exports it in the launcher.
func recoveryIdentity(state string) (*age.X25519Identity, error) {
	path := filepath.Join(state, recoveryKeyFile)
	if b, err := os.ReadFile(path); err == nil {
		return age.ParseX25519Identity(strings.TrimSpace(string(b)))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(id.String() + "\n")
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(path)
		return nil, err
	}
	return id, nil
}

func loadPoints(state string) ([]hostapi.RecoveryPoint, error) {
	b, err := os.ReadFile(filepath.Join(state, recoveryPointsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []hostapi.RecoveryPoint
	return list, json.Unmarshal(b, &list)
}

func savePoints(state string, list []hostapi.RecoveryPoint) error {
	if len(list) > maxStoredPoints {
		list = list[len(list)-maxStoredPoints:]
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	return atomicWrite(filepath.Join(state, recoveryPointsFile), append(b, '\n'), 0600)
}

func savePoint(state string, p hostapi.RecoveryPoint) error {
	list, err := loadPoints(state)
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].ID == p.ID {
			list[i] = p
			return savePoints(state, list)
		}
	}
	return savePoints(state, append(list, p))
}

func newPointID(now time.Time) string {
	var r [3]byte
	_, _ = rand.Read(r[:])
	return "rp-" + now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(r[:])
}

func findDestination(state, id string) (destinationRecord, error) {
	list, err := loadDestinations(state)
	if err != nil {
		return destinationRecord{}, err
	}
	for _, d := range list {
		if d.ID == id {
			return d, nil
		}
	}
	return destinationRecord{}, fmt.Errorf("unknown destination %q", id)
}

// queueRecovery records a cold recovery point to be written the next time the appliance
// is stopped. The caller then stops the appliance (triggerPower "restart-appliance"), and
// serve() writes the point between the two sessions.
func queueRecovery(state, destID, trigger string) (hostapi.RecoveryPoint, error) {
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	if _, err := os.Stat(filepath.Join(state, recoveryJobFile)); err == nil {
		return hostapi.RecoveryPoint{}, errors.New("a recovery point is already being written")
	}
	d, err := findDestination(state, destID)
	if err != nil {
		return hostapi.RecoveryPoint{}, err
	}
	got := checkDestination(state, d, time.Now())
	if !got.Available {
		return hostapi.RecoveryPoint{}, errors.New("the destination is not available: " + got.LastError)
	}
	need, err := allocatedBytes(filepath.Join(state, "data.img"))
	if err != nil {
		return hostapi.RecoveryPoint{}, err
	}
	need += need/10 + 256<<20
	if got.FreeBytes < need {
		return hostapi.RecoveryPoint{}, fmt.Errorf("the destination has %d MiB free; about %d MiB are needed", got.FreeBytes>>20, need>>20)
	}
	if _, err := recoveryIdentity(state); err != nil {
		return hostapi.RecoveryPoint{}, err
	}
	now := time.Now()
	p := hostapi.RecoveryPoint{ID: newPointID(now), Destination: destID, Trigger: trigger, Phase: hostapi.PointRunning,
		Consistency: "stopped", StartedAt: now.UTC().Format(time.RFC3339)}
	if err := savePoint(state, p); err != nil {
		return p, err
	}
	job, _ := json.Marshal(recoveryJob{ID: p.ID, Destination: destID, QueuedAt: p.StartedAt})
	if err := atomicWrite(filepath.Join(state, recoveryJobFile), job, 0600); err != nil {
		return p, err
	}
	return p, nil
}

// runRecoveryJob writes the queued point. It runs between appliance sessions, when the
// state lock is free and the disk is quiescent. A point is only published under its final
// name after it has been read back, so a partial file is never mistaken for a backup.
func runRecoveryJob(state string) {
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	raw, err := os.ReadFile(filepath.Join(state, recoveryJobFile))
	if err != nil {
		return
	}
	defer os.Remove(filepath.Join(state, recoveryJobFile))
	var job recoveryJob
	if json.Unmarshal(raw, &job) != nil {
		return
	}
	points, _ := loadPoints(state)
	var p hostapi.RecoveryPoint
	for _, q := range points {
		if q.ID == job.ID {
			p = q
		}
	}
	if p.ID == "" {
		return
	}
	fail := func(err error) {
		p.Phase, p.Error, p.FinishedAt = hostapi.PointFailed, trimTo(err.Error(), 240), time.Now().UTC().Format(time.RFC3339)
		_ = savePoint(state, p)
		fmt.Fprintln(os.Stderr, "recovery point failed:", err)
	}
	d, err := findDestination(state, job.Destination)
	if err != nil {
		fail(err)
		return
	}
	if got := checkDestination(state, d, time.Now()); !got.Available {
		fail(errors.New("the destination is not available: " + got.LastError))
		return
	}
	id, err := recoveryIdentity(state)
	if err != nil {
		fail(err)
		return
	}
	final := filepath.Join(d.Path, p.ID+".age")
	partial := final + ".partial"
	if err := createBackup(state, partial, id.Recipient()); err != nil {
		fail(err)
		return
	}
	sum, size, err := verifyBackup(partial, id)
	if err != nil {
		os.Remove(partial)
		fail(fmt.Errorf("the written copy did not read back: %w", err))
		return
	}
	if err := os.Rename(partial, final); err != nil {
		os.Remove(partial)
		fail(err)
		return
	}
	meta, _ := json.MarshalIndent(map[string]any{"id": p.ID, "createdAt": p.StartedAt, "consistency": "stopped", "sizeBytes": size,
		"sha256": sum, "appVersion": appVersion, "encryption": "age (X25519); the key is held by the launcher and is not in this folder",
		"note": "Whole-appliance cold copy. Not point-in-time recovery of any application."}, "", "  ")
	_ = os.WriteFile(filepath.Join(d.Path, p.ID+".json"), meta, 0600)
	if m, err := readManifest(filepath.Join(state, "release.json")); err == nil {
		p.ApplianceVersion, p.Arch = m.Version, m.Arch
	}
	if info, err := os.Stat(filepath.Join(state, "data.img")); err == nil {
		p.DataBytes = info.Size()
	}
	p.Phase, p.SizeBytes, p.SHA256, p.Verified, p.FinishedAt = hostapi.PointComplete, size, sum, true, time.Now().UTC().Format(time.RFC3339)
	_ = savePoint(state, p)
	applyRetention(state, d)
}

// verifyBackup reads the whole file back: it must decrypt, decompress and contain a
// complete tar with the data disk and release manifest. It returns the SHA-256 and size
// of the file as written.
func verifyBackup(file string, id *age.X25519Identity) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	dec, err := age.Decrypt(f, id)
	if err != nil {
		return "", 0, err
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		return "", 0, err
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, err
		}
		n, err := io.Copy(io.Discard, tr)
		if err != nil || n != hd.Size {
			return "", 0, errors.New("a member is truncated")
		}
		seen[hd.Name] = true
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return "", 0, err
	}
	if !seen["data.img"] || !seen["release.json"] {
		return "", 0, errors.New("the copy is incomplete")
	}
	return hex.EncodeToString(h.Sum(nil)), info.Size(), nil
}

// applyRetention is the only place the launcher deletes destination content. It removes
// only the points a schedule made, never one a person asked for, and keeps the newest
// KeepLast verified ones.
func applyRetention(state string, d destinationRecord) {
	sched, ok := loadSchedule(state)
	if !ok || !sched.Enabled || sched.Destination != d.ID || sched.KeepLast < 1 {
		return
	}
	points, _ := loadPoints(state)
	var mine []hostapi.RecoveryPoint
	for _, p := range points {
		if p.Destination == d.ID && p.Trigger == "schedule" && p.Phase == hostapi.PointComplete && p.Verified {
			mine = append(mine, p)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].FinishedAt > mine[j].FinishedAt })
	for _, p := range mine[min(len(mine), sched.KeepLast):] {
		if os.Remove(filepath.Join(d.Path, p.ID+".age")) != nil && fileExists(filepath.Join(d.Path, p.ID+".age")) {
			continue // still there; try again after the next point
		}
		_ = os.Remove(filepath.Join(d.Path, p.ID+".json"))
		p.Phase, p.Verified, p.Error = hostapi.PointFailed, false, "Removed by the schedule's retention"
		_ = savePoint(state, p)
	}
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// reconcileInterruptedRecovery runs when a launcher starts: a job left behind means the
// launcher died mid-write, so the point is reported as failed and any partial file removed.
func reconcileInterruptedRecovery(state string) {
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	raw, err := os.ReadFile(filepath.Join(state, recoveryJobFile))
	if err != nil {
		return
	}
	var job recoveryJob
	_ = json.Unmarshal(raw, &job)
	os.Remove(filepath.Join(state, recoveryJobFile))
	if d, err := findDestination(state, job.Destination); err == nil {
		os.Remove(filepath.Join(d.Path, job.ID+".age.partial"))
	}
	points, _ := loadPoints(state)
	for _, p := range points {
		if p.ID == job.ID && p.Phase == hostapi.PointRunning {
			p.Phase, p.Error, p.FinishedAt = hostapi.PointFailed, "The launcher stopped before the copy finished", time.Now().UTC().Format(time.RFC3339)
			_ = savePoint(state, p)
		}
	}
}

func loadSchedule(state string) (hostapi.BackupScheduleSpec, bool) {
	var s hostapi.BackupScheduleSpec
	b, err := os.ReadFile(filepath.Join(state, scheduleFile))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, false
	}
	return s, true
}

func saveSchedule(state string, s hostapi.BackupScheduleSpec) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return atomicWrite(filepath.Join(state, scheduleFile), append(b, '\n'), 0600)
}

func inWindow(now time.Time, startHour, hours int) bool {
	if hours >= 24 {
		return true
	}
	return (now.Hour()-startHour+24)%24 < hours
}

// newestSuccess is the finish time of the newest verified point for a destination.
func newestSuccess(points []hostapi.RecoveryPoint, dest string) (time.Time, bool) {
	var best time.Time
	for _, p := range points {
		if p.Destination != dest || p.Phase != hostapi.PointComplete || !p.Verified {
			continue
		}
		if t, err := time.Parse(time.RFC3339, p.FinishedAt); err == nil && t.After(best) {
			best = t
		}
	}
	return best, !best.IsZero()
}

func scheduleStatus(state string, now time.Time) *hostapi.BackupScheduleStatus {
	s, ok := loadSchedule(state)
	if !ok {
		return nil
	}
	st := &hostapi.BackupScheduleStatus{Enabled: s.Enabled, Destination: s.Destination, EveryHours: s.EveryHours, KeepLast: s.KeepLast,
		WindowStartHour: s.WindowStartHour, WindowHours: s.WindowHours}
	if !s.Enabled {
		return st
	}
	points, _ := loadPoints(state)
	for _, p := range points {
		if p.Destination == s.Destination && p.Trigger == "schedule" && p.StartedAt > st.LastAttempt {
			st.LastAttempt = p.StartedAt
		}
	}
	every := time.Duration(s.EveryHours) * time.Hour
	last, have := newestSuccess(points, s.Destination)
	due := now
	if have {
		st.LastSuccess = last.UTC().Format(time.RFC3339)
		due = last.Add(every)
	}
	st.NextDue = due.UTC().Format(time.RFC3339)
	grace := max(2*time.Hour, every/4)
	st.ObjectiveBreached = now.After(due.Add(grace)) || (!have && st.LastAttempt != "")
	if st.ObjectiveBreached {
		st.Note = "No verified recovery point inside the schedule's interval"
	}
	return st
}

// scheduleDue decides whether to start a scheduled point now.
func scheduleDue(state string, now time.Time) (string, bool) {
	s, ok := loadSchedule(state)
	if !ok || !s.Enabled {
		return "", false
	}
	if !inWindow(now, s.WindowStartHour, s.WindowHours) {
		return "", false
	}
	st := scheduleStatus(state, now)
	if st == nil {
		return "", false
	}
	if due, err := time.Parse(time.RFC3339, st.NextDue); err != nil || now.Before(due) {
		return "", false
	}
	if t, err := time.Parse(time.RFC3339, st.LastAttempt); err == nil && now.Sub(t) < time.Hour {
		return "", false // back off after a failed or recent attempt
	}
	return s.Destination, true
}

func recoveryKitFact(state string) *hostapi.RecoveryKit {
	k := &hostapi.RecoveryKit{}
	_, err := os.Stat(filepath.Join(state, recoveryKeyFile))
	k.KeyPresent = err == nil
	_, err = os.Stat(filepath.Join(state, keyExportedFile))
	k.KeyExported = err == nil
	switch {
	case !k.KeyPresent:
		k.Note = "No recovery key yet: it is created with the first recovery point."
	case !k.KeyExported:
		k.Note = "The key exists only on this computer. Without a copy kept elsewhere, these recovery points cannot be restored if this computer is lost."
	}
	return k
}

func recoveryFacts(state string) ([]hostapi.RecoveryPoint, *hostapi.BackupScheduleStatus, *hostapi.RecoveryKit) {
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	points, _ := loadPoints(state)
	if len(points) > 30 {
		points = points[len(points)-30:]
	}
	for i := range points {
		if points[i].Phase == hostapi.PointComplete && points[i].Verified {
			points[i].RestorePlan = restorePlan(state, points[i])
		}
	}
	return points, scheduleStatus(state, time.Now()), recoveryKitFact(state)
}

// restorePlan reports whether a point could be restored on this computer now. A restore
// goes into a new, empty state directory with the exact appliance the point was taken with;
// nothing here changes anything.
func restorePlan(state string, p hostapi.RecoveryPoint) *hostapi.RestorePlan {
	plan := &hostapi.RestorePlan{NeedsFreeBytes: p.DataBytes, ExpectedDataLoss: "Everything written after " + p.FinishedAt}
	block := func(s string) { plan.Blockers = append(plan.Blockers, s) }
	if d, err := findDestination(state, p.Destination); err != nil {
		block("The destination is no longer enrolled")
	} else if _, err := os.Stat(filepath.Join(d.Path, p.ID+".age")); err != nil {
		block("The recovery point file is not reachable. If it is on a removable disk, connect the disk.")
	}
	if _, err := os.Stat(filepath.Join(state, recoveryKeyFile)); err != nil {
		plan.Warnings = append(plan.Warnings, "The recovery key is not on this computer; restoring needs the copy you saved elsewhere")
	}
	if p.Arch != "" && p.Arch != runtime.GOARCH {
		block("This point was taken on " + p.Arch + "; a cold disk restore needs the same architecture (this computer is " + runtime.GOARCH + ")")
	}
	if u, err := diskspace.Of(state); err == nil && p.DataBytes > 0 && u.Free < uint64(p.DataBytes) {
		block(fmt.Sprintf("A restore writes the full %d GiB data disk; this computer has %d GiB free", p.DataBytes>>30, u.Free>>30))
	}
	if cur, err := readManifest(filepath.Join(state, "release.json")); err == nil && p.ApplianceVersion != "" {
		if cur.Version == p.ApplianceVersion {
			plan.Precedence = "This point matches the appliance now running (" + cur.Version + ")."
		} else {
			plan.Precedence = "This point needs appliance " + p.ApplianceVersion + " (" + cur.Version + " is running now). The matching appliance must be used for the restore; rolling the appliance back is a separate step that comes first."
		}
	}
	plan.Feasible = len(plan.Blockers) == 0
	return plan
}
