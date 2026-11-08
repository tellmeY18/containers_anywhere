package recoveryapi

import (
	"context"
	"errors"
)

func (c *Controller) restoreImported(ctx context.Context, r *Record, s RestoreSpec) error {
	if !safeID.MatchString(s.DurablePointID) || len(s.PVCNames) == 0 || len(s.PVCNames) > MaxMembers {
		return c.reject(r, "InvalidSpec", errors.New("import requires an opaque durablePointID and 1..64 PVC mappings"))
	}
	seen := map[string]bool{}
	for source, target := range s.PVCNames {
		if !dnsName(source) || !dnsName(target) || source == target || seen[target] {
			return c.reject(r, "InvalidMapping", errors.New("import requires unique new PVC names and valid source names"))
		}
		seen[target] = true
	}
	if err := c.checkNamespace(ctx, *r); err != nil {
		return err
	}
	if err := c.store().Put(*r); err != nil {
		return err
	}
	if c.RestoreImported == nil {
		r.Phase, r.Reason, r.Message = "Pending", "NotImplemented", "Imported workload restore adapter is not configured"
		return c.store().Put(*r)
	}
	op := ImportedRestoreRequest{RequestUID: r.UID, Namespace: r.Namespace, NamespaceUID: r.NamespaceUID, ServiceAccount: WorkloadServiceAccount, PointID: s.DurablePointID, PVCNames: s.PVCNames}
	st, err := c.RestoreImported(ctx, op)
	if err != nil {
		return err
	}
	switch st.Phase {
	case "Pending", "Running", "Succeeded", "Failed":
	default:
		return errors.New("import adapter returned invalid phase")
	}
	for source, target := range st.RestoredPVCs {
		if s.PVCNames[source] != target {
			return errors.New("import adapter returned an unrequested PVC mapping")
		}
	}
	if st.Phase == "Succeeded" && len(st.RestoredPVCs) != len(s.PVCNames) {
		return errors.New("import adapter success lacks a complete PVC mapping")
	}
	r.Phase, r.Reason, r.Message = st.Phase, st.Reason, st.Message
	r.RestoredPVCs = st.RestoredPVCs
	if terminal(r.Phase) {
		t := c.now()
		r.CompletedAt = &t
	}
	return c.store().Put(*r)
}
