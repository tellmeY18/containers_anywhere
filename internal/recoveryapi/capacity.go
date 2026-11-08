package recoveryapi

import "context"

// scheduleGate bounds materialized sets, including retained partial captures,
// exports in outage, active requests and deletion still waiting on CSI. Healthy
// rotation gets one replacement slot beyond keepLast; blocked sets alone may
// not consume more than keepLast. No cursor advances while this gate is closed.
func (c *Controller) scheduleGate(ctx context.Context, policy *Record, keep int) (string, string, error) {
	all, err := c.store().List()
	if err != nil {
		return "", "", err
	}
	if policy.AwaitingCapture != "" {
		seen := false
		for _, point := range all {
			if (point.Kind == "RecoveryPoint" || point.Kind == "ClusterRecoveryPoint") && point.Name == policy.AwaitingCapture && point.Namespace == policy.Namespace {
				seen = true
				break
			}
		}
		if !seen {
			return "CaptureBacklog", "The prior scheduled request has not yet been observed; no additional capture is queued", nil
		}
		policy.AwaitingCapture = ""
	}
	live, blocked := 0, 0
	for _, point := range all {
		if point.PolicyUID != policy.UID || (point.Kind != "RecoveryPoint" && point.Kind != "ClusterRecoveryPoint") || point.Phase == "Pruned" {
			continue
		}
		// Failed validation/capacity checks created no data. Partial captures,
		// uncertain backend submissions and historical sets stay in the budget.
		if point.Phase == "Failed" && len(point.Members) == 0 && len(point.Hooks) == 0 && !point.BackendAttempted && !point.BackendSubmitted && point.RecoveryPointID == "" && !point.ResumeUncertain {
			continue
		}
		live++
		if point.ExportPending || len(point.ProtectedBy) > 0 || point.ResumeUncertain || point.Phase == "Failed" || point.Phase == "ResumeFailed" || (point.Phase != "Succeeded" && point.Phase != "Pruning") {
			blocked++
		}
	}
	if keep < 1 || live >= keep+1 {
		return "RetentionBlocked", "Live and retiring sets fill keepLast plus its single replacement slot; capture waits for safe retention cleanup", nil
	}
	if blocked >= keep {
		return "CaptureBacklog", "Export-pending, protected, partial or active sets fill the keepLast backlog budget; capture waits without deleting protected data", nil
	}
	if c.CheckCapacity != nil {
		if err := c.CheckCapacity(ctx); err != nil {
			return "StoragePressure", "Capture waits for host and guest storage reserves to recover; the schedule cursor is unchanged", nil
		}
	}
	return "", "", nil
}
