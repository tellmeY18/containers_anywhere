package recoveryapi

import (
	"context"
	"encoding/json"
	"strings"
)

func historicalStatus(st *Status) bool {
	return st != nil && (st.Phase != "" || st.ObservedGeneration != 0 || st.Reason != "" || len(st.Conditions) > 0 || len(st.Members) > 0 || len(st.RestoredPVCs) > 0 || st.RecoveryPointID != "")
}

// Kubernetes status is evidence AGAINST replay, never enough authorization to
// reconstruct a missing hook preparation journal or prune a historical point.
func (c *Controller) quarantineHistorical(ctx context.Context, o Object, r *Record) error {
	r.Phase, r.Reason, r.Message = "Failed", "HostJournalMissing", "Historical request has no host preparation/resume journal. Replay is blocked; inspect quiescence and perform authorized resume checks before creating a new request UID."
	r.ProtectedBy = []string{"historical-unverified"}
	r.Members = o.Status.Members
	r.RestoredPVCs = o.Status.RestoredPVCs
	r.RecoveryPointID = o.Status.RecoveryPointID
	r.ResumeUncertain = o.Kind == "RecoveryPoint" && o.Status.Phase != "Succeeded"
	var spec struct {
		Scope string `json:"scope"`
	}
	_ = json.Unmarshal(o.Spec, &spec)
	r.Scope = spec.Scope
	if strings.HasPrefix(o.Kind, "Cluster") && c.Backend != nil && validateScope(r.Scope, true) == nil {
		// Get only: never Submit, even when the guest has forgotten this UID.
		if st, err := c.Backend.Get(ctx, r.UID); err == nil && (st.Phase == "Succeeded" || st.Phase == "Failed") {
			if o.Kind != "ClusterRecoveryPoint" || st.Phase != "Succeeded" || st.RecoveryPointID != "" {
				r.Phase, r.Reason, r.Message = st.Phase, "GuestJournalRecovered", "Terminal result recovered by UID from the guest journal; historical record remains protected from automatic retention"
				r.BackendSubmitted = true
				r.RecoveryPointID = st.RecoveryPointID
			}
		}
	}
	t := c.now()
	r.CompletedAt = &t
	if err := c.store().Put(*r); err != nil {
		return err
	}
	return c.patchStatus(ctx, o, statusFor(o, *r))
}
