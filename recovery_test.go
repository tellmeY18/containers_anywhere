package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/hostapi"
)

func recoveryFixture(t *testing.T) (state string, dest destinationRecord) {
	t.Helper()
	state = t.TempDir()
	os.WriteFile(filepath.Join(state, "data.img"), []byte("synthetic disk contents"), 0600)
	m, _ := json.Marshal(manifest{Version: "test", Arch: runtime.GOARCH})
	os.WriteFile(filepath.Join(state, "release.json"), m, 0600)
	rec, err := addDestination(state, "Backup disk", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return state, rec
}

func TestARecoveryPointIsWrittenVerifiedAndRestorable(t *testing.T) {
	state, dest := recoveryFixture(t)
	p, err := queueRecovery(state, dest.ID, "request")
	if err != nil {
		t.Fatal(err)
	}
	if p.Phase != hostapi.PointRunning || p.Consistency != "stopped" {
		t.Fatalf("%+v", p)
	}
	if _, err := queueRecovery(state, dest.ID, "request"); err == nil {
		t.Fatal("two recovery points must not be queued at once")
	}
	runRecoveryJob(state)
	points, _ := loadPoints(state)
	if len(points) != 1 || points[0].Phase != hostapi.PointComplete || !points[0].Verified || points[0].SizeBytes <= 0 || points[0].SHA256 == "" {
		t.Fatalf("%+v", points)
	}
	if _, err := os.Stat(filepath.Join(dest.Path, p.ID+".age")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest.Path, p.ID+".age.partial")); err == nil {
		t.Fatal("a partial file must not remain")
	}
	if _, err := os.Stat(filepath.Join(state, recoveryJobFile)); err == nil {
		t.Fatal("the job must be cleared")
	}
	if b, _ := os.ReadFile(filepath.Join(dest.Path, p.ID+".json")); strings.Contains(string(b), "synthetic") || !strings.Contains(string(b), "sha256") {
		t.Fatalf("metadata: %s", b)
	}
	// The key never goes to the destination, and the copy restores with it.
	entries, _ := os.ReadDir(dest.Path)
	for _, e := range entries {
		if strings.Contains(e.Name(), "key") {
			t.Fatal("the key must not be in the destination")
		}
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err := restore(target, filepath.Join(dest.Path, p.ID+".age"), filepath.Join(state, recoveryKeyFile)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "data.img")); string(got) != "synthetic disk contents" {
		t.Fatal("restore did not return the data")
	}
}

func TestAFailedWriteIsReportedAndLeavesNothingBehind(t *testing.T) {
	state, dest := recoveryFixture(t)
	p, err := queueRecovery(state, dest.ID, "request")
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(dest.Path) // the disk is pulled before the copy starts
	runRecoveryJob(state)
	points, _ := loadPoints(state)
	if points[0].Phase != hostapi.PointFailed || points[0].Error == "" || points[0].Verified {
		t.Fatalf("%+v", points[0])
	}
	if _, err := os.Stat(filepath.Join(state, recoveryJobFile)); err == nil {
		t.Fatal("the job must be cleared after a failure")
	}
	_ = p
}

func TestAnUnavailableDestinationIsRefusedBeforeTheApplianceStops(t *testing.T) {
	state, dest := recoveryFixture(t)
	os.RemoveAll(dest.Path)
	if _, err := queueRecovery(state, dest.ID, "request"); err == nil {
		t.Fatal("must be refused while the destination is missing")
	}
	if _, err := queueRecovery(state, "nope", "request"); err == nil {
		t.Fatal("an unknown destination must be refused")
	}
	if _, err := os.Stat(filepath.Join(state, recoveryJobFile)); err == nil {
		t.Fatal("nothing may be queued")
	}
}

func TestALauncherCrashMidWriteIsReportedOnTheNextStart(t *testing.T) {
	state, dest := recoveryFixture(t)
	p, _ := queueRecovery(state, dest.ID, "request")
	os.WriteFile(filepath.Join(dest.Path, p.ID+".age.partial"), []byte("half"), 0600)
	reconcileInterruptedRecovery(state)
	points, _ := loadPoints(state)
	if points[0].Phase != hostapi.PointFailed {
		t.Fatalf("%+v", points[0])
	}
	if _, err := os.Stat(filepath.Join(dest.Path, p.ID+".age.partial")); err == nil {
		t.Fatal("the partial file must be removed")
	}
}

func TestRetentionRemovesOnlyOldScheduledPoints(t *testing.T) {
	state, dest := recoveryFixture(t)
	saveSchedule(state, hostapi.BackupScheduleSpec{Enabled: true, Destination: dest.ID, EveryHours: 6, KeepLast: 2, WindowHours: 24})
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id, trigger string, day int) {
		os.WriteFile(filepath.Join(dest.Path, id+".age"), []byte("x"), 0600)
		savePoint(state, hostapi.RecoveryPoint{ID: id, Destination: dest.ID, Trigger: trigger, Phase: hostapi.PointComplete, Consistency: "stopped",
			StartedAt: base.AddDate(0, 0, day).Format(time.RFC3339), FinishedAt: base.AddDate(0, 0, day).Format(time.RFC3339), Verified: true})
	}
	mk("s1", "schedule", 1)
	mk("s2", "schedule", 2)
	mk("s3", "schedule", 3)
	mk("manual", "request", 0)
	applyRetention(state, dest)
	for id, want := range map[string]bool{"s1": false, "s2": true, "s3": true, "manual": true} {
		if _, err := os.Stat(filepath.Join(dest.Path, id+".age")); (err == nil) != want {
			t.Errorf("%s present=%v want %v", id, err == nil, want)
		}
	}
}

func TestScheduleHonoursWindowIntervalBackoffAndReportsBreach(t *testing.T) {
	state, dest := recoveryFixture(t)
	saveSchedule(state, hostapi.BackupScheduleSpec{Enabled: true, Destination: dest.ID, EveryHours: 24, KeepLast: 3, WindowStartHour: 2, WindowHours: 4})
	day := time.Date(2026, 10, 10, 3, 0, 0, 0, time.Local)
	if d, ok := scheduleDue(state, day); !ok || d != dest.ID {
		t.Fatal("never run and inside the window: due now")
	}
	if _, ok := scheduleDue(state, day.Add(5*time.Hour)); ok {
		t.Fatal("outside the window must not run")
	}
	savePoint(state, hostapi.RecoveryPoint{ID: "a", Destination: dest.ID, Trigger: "schedule", Phase: hostapi.PointComplete, Consistency: "stopped",
		StartedAt: day.Format(time.RFC3339), FinishedAt: day.Format(time.RFC3339), Verified: true})
	if _, ok := scheduleDue(state, day.Add(3*time.Hour)); ok {
		t.Fatal("not due again inside the interval")
	}
	next := day.Add(24 * time.Hour)
	if _, ok := scheduleDue(state, next); !ok {
		t.Fatal("due after the interval")
	}
	if st := scheduleStatus(state, next); st.ObjectiveBreached {
		t.Fatal("exactly due is not a breach")
	}
	if st := scheduleStatus(state, next.Add(30*time.Hour)); !st.ObjectiveBreached {
		t.Fatal("a long overdue schedule must be reported as breached")
	}
	savePoint(state, hostapi.RecoveryPoint{ID: "b", Destination: dest.ID, Trigger: "schedule", Phase: hostapi.PointFailed, Consistency: "stopped",
		StartedAt: next.Format(time.RFC3339), FinishedAt: next.Format(time.RFC3339)})
	if _, ok := scheduleDue(state, next.Add(10*time.Minute)); ok {
		t.Fatal("must back off after a recent attempt")
	}
	saveSchedule(state, hostapi.BackupScheduleSpec{})
	if _, ok := scheduleDue(state, next); ok {
		t.Fatal("a cancelled schedule never runs")
	}
}

func TestTheRecoveryKeyIsCreatedOnceAndOnlyExportedByTheLauncher(t *testing.T) {
	state := t.TempDir()
	if k := recoveryKitFact(state); k.KeyPresent || k.KeyExported {
		t.Fatalf("%+v", k)
	}
	a, err := recoveryIdentity(state)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := recoveryIdentity(state)
	if a.String() != b.String() {
		t.Fatal("the key must be created once and reused")
	}
	if info, _ := os.Stat(filepath.Join(state, recoveryKeyFile)); info.Mode().Perm() != 0600 {
		t.Fatal("the key file must be private")
	}
	k := recoveryKitFact(state)
	if !k.KeyPresent || k.KeyExported || !strings.Contains(k.Note, "only on this computer") {
		t.Fatalf("%+v", k)
	}
	if strings.Contains(strings.ToLower(k.Note), a.String()) {
		t.Fatal("the key must never be published")
	}
}

func TestRestorePlanReportsBlockersAndPrecedence(t *testing.T) {
	state, dest := recoveryFixture(t)
	queueRecovery(state, dest.ID, "request")
	runRecoveryJob(state)
	points, _ := loadPoints(state)
	p := points[0]
	if p.ApplianceVersion != "test" || p.Arch != runtime.GOARCH || p.DataBytes <= 0 {
		t.Fatalf("a point must record what a restore needs: %+v", p)
	}
	plan := restorePlan(state, p)
	if !plan.Feasible || len(plan.Blockers) != 0 || !strings.Contains(plan.ExpectedDataLoss, p.FinishedAt) || !strings.Contains(plan.Precedence, "matches") {
		t.Fatalf("%+v", plan)
	}
	q := p
	q.Arch = "other"
	if plan := restorePlan(state, q); plan.Feasible || !strings.Contains(strings.Join(plan.Blockers, " "), "architecture") {
		t.Fatalf("%+v", plan)
	}
	q = p
	q.ApplianceVersion = "9.9.9"
	if plan := restorePlan(state, q); !plan.Feasible || !strings.Contains(plan.Precedence, "needs appliance 9.9.9") {
		t.Fatalf("%+v", plan)
	}
	q = p
	q.DataBytes = 1 << 60
	if plan := restorePlan(state, q); plan.Feasible || !strings.Contains(strings.Join(plan.Blockers, " "), "free") {
		t.Fatalf("%+v", plan)
	}
	os.Remove(filepath.Join(dest.Path, p.ID+".age"))
	if plan := restorePlan(state, p); plan.Feasible {
		t.Fatalf("a missing file must block: %+v", plan)
	}
	os.Remove(filepath.Join(state, recoveryKeyFile))
	if plan := restorePlan(state, p); len(plan.Warnings) == 0 {
		t.Fatal("a missing key must be warned about")
	}
}
