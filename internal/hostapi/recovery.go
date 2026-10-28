package hostapi

import (
	"context"
	"errors"
	"time"
)

const (
	RecoveryPointResource = "hostrecoverypoints." + Group

	MinScheduleHours  = 6
	MaxScheduleHours  = 24 * 30
	MaxKeepLast       = 30
	maxRecoveryPoints = 30
)

// RecoveryPointSpec asks for one cold recovery point. It needs approval in the launcher
// because the appliance is stopped while it is written.
type RecoveryPointSpec struct {
	Destination string `json:"destination"`
}

// BackupScheduleSpec asks for cold recovery points on a timetable. Approving it is the consent
// for the downtime: every point stops the appliance, but only inside the window (the
// launcher computer's local time). Enabled=false cancels the schedule.
type BackupScheduleSpec struct {
	Enabled         bool   `json:"enabled"`
	Destination     string `json:"destination,omitempty"`
	EveryHours      int    `json:"everyHours,omitempty"`
	KeepLast        int    `json:"keepLast,omitempty"`
	WindowStartHour int    `json:"windowStartHour,omitempty"`
	WindowHours     int    `json:"windowHours,omitempty"`
}

// Recovery point phases.
const (
	PointRunning  = "Running"
	PointComplete = "Complete"
	PointFailed   = "Failed"
)

// RecoveryPoint reports one whole-appliance copy. Consistency is what was actually
// achieved, never what was hoped for. This is not point-in-time recovery of any
// application: it is the disk as it was when the appliance was stopped.
type RecoveryPoint struct {
	ID          string `json:"id"`
	Destination string `json:"destination"`
	Trigger     string `json:"trigger"` // "request" or "schedule"
	Phase       string `json:"phase"`
	Consistency string `json:"consistency"` // "stopped"
	StartedAt   string `json:"startedAt"`
	FinishedAt  string `json:"finishedAt,omitempty"`
	SizeBytes   int64  `json:"sizeBytes,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Verified    bool   `json:"verified"`
	Error       string `json:"error,omitempty"`
	// What a restore of this point needs to match.
	ApplianceVersion string       `json:"applianceVersion,omitempty"`
	Arch             string       `json:"arch,omitempty"`
	DataBytes        int64        `json:"dataBytes,omitempty"` // the disk's size
	UsedBytes        int64        `json:"usedBytes,omitempty"` // what it really occupied when copied
	RestorePlan      *RestorePlan `json:"restorePlan,omitempty"`
}

// RestorePlan is the launcher's honest answer to "could this point be restored here, and
// at what cost?". It is computed, not promised: restoring is never automatic.
type RestorePlan struct {
	Feasible         bool     `json:"feasible"`
	Blockers         []string `json:"blockers,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	NeedsFreeBytes   int64    `json:"needsFreeBytes"`
	ExpectedDataLoss string   `json:"expectedDataLoss"`
	// Precedence says which appliance a restore needs relative to the one running now.
	Precedence string `json:"precedence,omitempty"`
}

func (p RecoveryPoint) validate() error {
	if !destinationID.MatchString(p.ID) || !destinationID.MatchString(p.Destination) || !validString(p.Trigger) ||
		!validString(p.Phase) || !validString(p.Consistency) || !validString(p.StartedAt) || !validString(p.FinishedAt) ||
		!validString(p.SHA256) || !validString(p.Error) || !validString(p.ApplianceVersion) || !validString(p.Arch) || p.SizeBytes < 0 || p.DataBytes < 0 || p.UsedBytes < 0 || !validPlan(p.RestorePlan) {
		return errors.New("invalid recovery point")
	}
	switch p.Phase {
	case PointRunning, PointComplete, PointFailed:
		return nil
	}
	return errors.New("invalid recovery point phase")
}

// BackupScheduleStatus is the schedule as the launcher holds it, with what actually happened.
type BackupScheduleStatus struct {
	Enabled         bool   `json:"enabled"`
	Destination     string `json:"destination,omitempty"`
	EveryHours      int    `json:"everyHours,omitempty"`
	KeepLast        int    `json:"keepLast,omitempty"`
	WindowStartHour int    `json:"windowStartHour,omitempty"`
	WindowHours     int    `json:"windowHours,omitempty"`
	LastSuccess     string `json:"lastSuccess,omitempty"`
	LastAttempt     string `json:"lastAttempt,omitempty"`
	NextDue         string `json:"nextDue,omitempty"`
	// ObjectiveBreached is true when the newest verified point is older than the interval
	// (the destination was offline, or the window never opened).
	ObjectiveBreached bool   `json:"objectiveBreached"`
	Note              string `json:"note,omitempty"`
}

// RecoveryKit says whether a restore is possible without this computer's help. The key itself
// is never published: exporting it is a launcher-only act.
type RecoveryKit struct {
	KeyPresent  bool   `json:"keyPresent"`
	KeyExported bool   `json:"keyExported"`
	Note        string `json:"note,omitempty"`
}

func validateRecovery(f Facts) error {
	if len(f.RecoveryPoints) > maxRecoveryPoints {
		return errors.New("too many recovery points")
	}
	seen := map[string]bool{}
	for _, p := range f.RecoveryPoints {
		if err := p.validate(); err != nil || seen[p.ID] {
			return errors.New("invalid recovery point")
		}
		seen[p.ID] = true
	}
	if s := f.BackupSchedule; s != nil {
		for _, v := range []string{s.Destination, s.LastSuccess, s.LastAttempt, s.NextDue, s.Note} {
			if !validString(v) {
				return errors.New("invalid backup schedule")
			}
		}
	}
	if k := f.RecoveryKit; k != nil && !validString(k.Note) {
		return errors.New("invalid recovery kit")
	}
	return nil
}

func pointState(p RecoveryPoint) any {
	cond := Condition{Type: "Complete", Status: "False", Reason: p.Phase, Message: "The recovery point is being written"}
	switch p.Phase {
	case PointComplete:
		cond = Condition{Type: "Complete", Status: "True", Reason: "Verified", Message: "Written and read back successfully"}
		if !p.Verified {
			cond = Condition{Type: "Complete", Status: "True", Reason: "NotVerified", Message: "Written, but not read back"}
		}
	case PointFailed:
		cond = Condition{Type: "Complete", Status: "False", Reason: "Failed", Message: p.Error}
	}
	return struct {
		RecoveryPoint
		Conditions []Condition `json:"conditions"`
	}{p, []Condition{cond}}
}

// ReconcileRecoveryPoints publishes one read-only HostRecoveryPoint per point the launcher reports.
func (c *Controller) ReconcileRecoveryPoints(ctx context.Context) error {
	c.mu.RLock()
	facts, at := c.facts, c.factsAt
	c.mu.RUnlock()
	now := c.now()
	if facts == nil || now.Sub(at) > StaleAfter {
		return nil
	}
	key := recoveryKey(facts.RecoveryPoints)
	c.mu.Lock()
	skip := c.pointsWritten && key == c.pointsKey && now.Sub(c.pointsWrite) < 30*time.Second
	c.mu.Unlock()
	if skip {
		return nil
	}
	var objs []publishedObject
	for _, p := range facts.RecoveryPoints {
		objs = append(objs, publishedObject{Name: p.ID, Status: pointState(p)})
	}
	if err := c.reconcileObjects(ctx, RecoveryPointResource, "HostRecoveryPoint", objs); err != nil {
		return err
	}
	c.mu.Lock()
	c.pointsWritten, c.pointsKey, c.pointsWrite = true, key, now
	c.mu.Unlock()
	return nil
}

func validPlan(r *RestorePlan) bool {
	if r == nil {
		return true
	}
	if len(r.Blockers) > 8 || len(r.Warnings) > 8 || r.NeedsFreeBytes < 0 || !validString(r.ExpectedDataLoss) || !validString(r.Precedence) {
		return false
	}
	for _, v := range append(append([]string{}, r.Blockers...), r.Warnings...) {
		if !validString(v) {
			return false
		}
	}
	return true
}
