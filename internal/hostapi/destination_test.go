package hostapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func dest(id string) Destination {
	return Destination{ID: id, Name: "Backup disk", Kind: "folder", Available: true, FreeBytes: 10 << 30, TotalBytes: 100 << 30, CheckedAt: "2026-10-28T10:00:00Z"}
}

func TestDestinationValidation(t *testing.T) {
	f := goodFacts(time.Now())
	f.Destinations = []Destination{dest("usb-1")}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Facts){
		"bad id":    func(f *Facts) { f.Destinations[0].ID = "../x" },
		"empty":     func(f *Facts) { f.Destinations[0].Name = "" },
		"duplicate": func(f *Facts) { f.Destinations = append(f.Destinations, dest("usb-1")) },
		"too many": func(f *Facts) {
			f.Destinations = nil
			for i := 0; i < 17; i++ {
				f.Destinations = append(f.Destinations, dest("d"+strings.Repeat("a", i)))
			}
		},
	} {
		g := goodFacts(time.Now())
		g.Destinations = []Destination{dest("usb-1")}
		mutate(&g)
		if g.Validate() == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestDestinationsArePublishedPrunedAndNeverFromStaleFacts(t *testing.T) {
	k := &fakeKube{get: map[string]string{DestinationResource: `{"items":[{"metadata":{"name":"usb-1"}},{"metadata":{"name":"gone"}}]}`}}
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	c := newController(k, &now)
	f := goodFacts(now)
	f.Destinations = []Destination{dest("usb-1")}
	if err := c.SetFacts(f); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileDestinations(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sawApply, sawPatch, sawDeleteGone, sawDeleteKept bool
	for _, call := range k.calls {
		j := strings.Join(call.args, " ")
		switch {
		case call.args[0] == "apply" && strings.Contains(call.input, `"kind":"HostDestination"`):
			sawApply = true
		case call.args[0] == "patch" && strings.Contains(j, "--subresource=status") && strings.Contains(j, "usb-1"):
			sawPatch = true
		case call.args[0] == "delete" && strings.Contains(j, "gone"):
			sawDeleteGone = true
		case call.args[0] == "delete" && strings.Contains(j, "usb-1"):
			sawDeleteKept = true
		}
	}
	if !sawApply || !sawPatch || !sawDeleteGone || sawDeleteKept {
		t.Fatalf("apply=%v patch=%v deleteGone=%v deleteKept=%v", sawApply, sawPatch, sawDeleteGone, sawDeleteKept)
	}
	n := len(k.calls)
	now = now.Add(5 * time.Second)
	f.HostTime = now.Format(time.RFC3339)
	f.Destinations[0].CheckedAt = "2026-10-28T10:00:05Z"
	_ = c.SetFacts(f)
	if err := c.ReconcileDestinations(context.Background()); err != nil || len(k.calls) != n {
		t.Fatal("a changed check time alone must not rewrite the objects")
	}
	// Stale facts: nothing is published or deleted.
	now = now.Add(5 * time.Minute)
	if err := c.ReconcileDestinations(context.Background()); err != nil || len(k.calls) != n {
		t.Fatal("stale facts must not be acted on")
	}
}

func TestDestinationCarriesNoHostPath(t *testing.T) {
	b, _ := json.Marshal(destinationState(dest("usb-1")))
	if strings.Contains(strings.ToLower(string(b)), "path") {
		t.Fatalf("a host path must never be published: %s", b)
	}
}

func TestAnUnavailableDestinationReportsWhy(t *testing.T) {
	d := dest("usb-1")
	d.Available, d.LastError = false, "The folder is not there."
	st := destinationState(d)
	if st.Conditions[0].Status != "False" || st.Conditions[0].Message != "The folder is not there." {
		t.Fatalf("%+v", st.Conditions)
	}
}

func TestRecoveryRequestValidation(t *testing.T) {
	good := []RequestSpec{
		{Type: TypeRecoveryPoint, RecoveryPoint: &RecoveryPointSpec{Destination: "usb-1"}},
		{Type: TypeBackupSchedule, BackupSchedule: &BackupScheduleSpec{Enabled: true, Destination: "usb-1", EveryHours: 24, KeepLast: 7, WindowStartHour: 2, WindowHours: 4}},
		{Type: TypeBackupSchedule, BackupSchedule: &BackupScheduleSpec{Enabled: false}},
	}
	for _, s := range good {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := []RequestSpec{
		{Type: TypeRecoveryPoint},
		{Type: TypeRecoveryPoint, RecoveryPoint: &RecoveryPointSpec{Destination: "../x"}},
		{Type: TypeRecoveryPoint, RecoveryPoint: &RecoveryPointSpec{Destination: "usb-1"}, Power: &PowerSpec{Action: "shutdown"}},
		{Type: TypeBackupSchedule, BackupSchedule: &BackupScheduleSpec{Enabled: true, Destination: "usb-1", EveryHours: 1, KeepLast: 7, WindowHours: 4}},
		{Type: TypeBackupSchedule, BackupSchedule: &BackupScheduleSpec{Enabled: true, Destination: "usb-1", EveryHours: 24, KeepLast: 0, WindowHours: 4}},
		{Type: TypeBackupSchedule, BackupSchedule: &BackupScheduleSpec{Enabled: true, Destination: "usb-1", EveryHours: 24, KeepLast: 3, WindowStartHour: 24, WindowHours: 4}},
		{Type: TypeBackupSchedule, BackupSchedule: &BackupScheduleSpec{Enabled: true, Destination: "usb-1", EveryHours: 24, KeepLast: 3}},
	}
	for _, s := range bad {
		if s.Validate() == nil {
			t.Errorf("must be refused: %+v", s)
		}
	}
}

func TestRecoveryPointsArePublishedFromFreshFactsOnly(t *testing.T) {
	k := &fakeKube{get: map[string]string{RecoveryPointResource: `{"items":[{"metadata":{"name":"old"}}]}`}}
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	c := newController(k, &now)
	f := goodFacts(now)
	f.RecoveryPoints = []RecoveryPoint{{ID: "rp-1", Destination: "usb-1", Trigger: "request", Phase: PointComplete, Consistency: "stopped", StartedAt: "2026-10-28T09:00:00Z", Verified: true}}
	if err := c.SetFacts(f); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileRecoveryPoints(context.Background()); err != nil {
		t.Fatal(err)
	}
	var applied, deletedOld bool
	for _, call := range k.calls {
		if call.args[0] == "apply" && strings.Contains(call.input, `"kind":"HostRecoveryPoint"`) {
			applied = true
		}
		if call.args[0] == "delete" && strings.Contains(strings.Join(call.args, " "), "old") {
			deletedOld = true
		}
	}
	if !applied || !deletedOld {
		t.Fatalf("applied=%v deletedOld=%v", applied, deletedOld)
	}
	bad := goodFacts(now)
	bad.RecoveryPoints = []RecoveryPoint{{ID: "rp-1", Destination: "usb-1", Trigger: "request", Phase: "Bogus", Consistency: "stopped", StartedAt: "x"}}
	if bad.Validate() == nil {
		t.Fatal("an unknown phase must be refused")
	}
}
