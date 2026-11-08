package recoveryapi

import (
	"context"
	"encoding/json"
	"errors"
)

func (c *Controller) destination(ctx context.Context, name, namespace string) (Destination, error) {
	if !dnsName(name) {
		return Destination{}, errors.New("invalid destination name")
	}
	b, err := c.run(ctx, nil, "get", "backupdestinations."+Group, name, "-o", "json")
	if err != nil {
		return Destination{}, err
	}
	var o struct {
		Metadata Metadata        `json:"metadata"`
		Spec     DestinationSpec `json:"spec"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return Destination{}, err
	}
	if o.Metadata.UID == "" || o.Metadata.DeletionTimestamp != nil {
		return Destination{}, errors.New("destination missing or deleting")
	}
	if namespace != "" {
		allowed := false
		for _, ns := range o.Spec.AllowedNamespaces {
			if ns == namespace {
				allowed = true
			}
		}
		if !allowed {
			return Destination{}, errors.New("destination does not explicitly authorize this namespace")
		}
	}
	return Destination{Name: name, UID: o.Metadata.UID, Spec: o.Spec}, nil
}

func (c *Controller) exportPoint(ctx context.Context, r *Record) error {
	if !r.ExportPending || r.Phase != "Succeeded" {
		return nil
	}
	finish := func(st ExportStatus) error {
		r.Export, r.ExportReason = st, st.Reason
		r.ExportPending = st.Phase != "Succeeded"
		return c.store().Put(*r)
	}
	if r.Namespace != "" {
		if err := c.checkNamespace(ctx, *r); err != nil {
			return finish(ExportStatus{Phase: "Pending", Reason: "NamespaceUnavailable", Message: "Namespace identity no longer exists; retained point needs administrator export"})
		}
	}
	d, err := c.destination(ctx, r.Destination, r.Namespace)
	if err != nil {
		return finish(ExportStatus{Phase: "Pending", Reason: "DestinationUnavailable", Message: "Destination unavailable or namespace authorization revoked"})
	}
	if d.UID != r.DestinationUID {
		return finish(ExportStatus{Phase: "Failed", Reason: "DestinationChanged", Message: "Destination UID differs from the capture journal"})
	}
	ready, err := c.Destination(ctx, d.Spec)
	if err != nil {
		return err
	}
	allowed := false
	reason := "DestinationNotReady"
	for _, condition := range ready.Conditions {
		if condition.Type == "Ready" {
			allowed = condition.Status == "True"
			reason = condition.Reason
		}
	}
	if !allowed {
		return finish(ExportStatus{Phase: "Pending", Reason: reason, Message: "Export waits for an enrolled and supported destination"})
	}
	if c.Export == nil {
		return finish(ExportStatus{Phase: "Pending", Reason: "NotImplemented", Message: "Export adapter is not configured"})
	}
	st, err := c.Export(ctx, *r, d)
	if err != nil {
		return finish(ExportStatus{Phase: "Failed", Reason: "ExportFailed", Message: "Export adapter failed; retained data is protected and export will retry"})
	}
	switch st.Phase {
	case "Pending", "Running", "Succeeded", "Failed":
	default:
		return errors.New("export adapter returned invalid phase")
	}
	if st.Phase == "Succeeded" && st.Receipt == "" {
		return errors.New("export completion requires an opaque authenticated receipt")
	}
	return finish(st)
}

// Only metadata identity is read with controller authority. It is never used to
// exec or read arbitrary workload data. Backend registration revalidates the
// actual btrfs CSI driver state before opening the referenced snapshot.
func (c *Controller) resolveVolumes(ctx context.Context, r *Record) error {
	for i := range r.Members {
		m := &r.Members[i]
		if !dnsName(m.VolumeName) {
			return errors.New("bound PVC lacks a valid PV name")
		}
		b, err := c.run(ctx, nil, "get", "persistentvolume", m.VolumeName, "-o", "json")
		if err != nil {
			return err
		}
		var pv struct {
			Spec struct {
				ClaimRef struct {
					UID       string `json:"uid"`
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"claimRef"`
				CSI struct {
					Driver       string `json:"driver"`
					VolumeHandle string `json:"volumeHandle"`
				} `json:"csi"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(b, &pv); err != nil {
			return err
		}
		if pv.Spec.ClaimRef.UID != m.UID || pv.Spec.ClaimRef.Name != m.PVC || pv.Spec.ClaimRef.Namespace != r.Namespace || pv.Spec.CSI.Driver != "btrfs.csi.local" || pv.Spec.CSI.VolumeHandle == "" {
			return errors.New("PV claim/driver identity mismatch")
		}
		m.VolumeHandle = pv.Spec.CSI.VolumeHandle
	}
	return nil
}

func (c *Controller) resolveSnapshot(ctx context.Context, r Record, m *Member) error {
	b, err := c.run(ctx, nil, "get", "volumesnapshotcontent", m.Content, "-o", "json")
	if err != nil {
		return err
	}
	var content struct {
		Metadata Metadata `json:"metadata"`
		Spec     struct {
			Driver            string `json:"driver"`
			DeletionPolicy    string `json:"deletionPolicy"`
			VolumeSnapshotRef struct {
				UID       string `json:"uid"`
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"volumeSnapshotRef"`
			Source struct {
				VolumeHandle string `json:"volumeHandle"`
			} `json:"source"`
		} `json:"spec"`
		Status struct {
			Ready          bool   `json:"readyToUse"`
			SnapshotHandle string `json:"snapshotHandle"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &content); err != nil {
		return err
	}
	ref := content.Spec.VolumeSnapshotRef
	if content.Spec.Driver != "btrfs.csi.local" || content.Spec.DeletionPolicy != "Retain" || ref.UID != m.SnapshotUID || ref.Name != m.Snapshot || ref.Namespace != r.Namespace || content.Spec.Source.VolumeHandle != m.VolumeHandle || !content.Status.Ready || content.Status.SnapshotHandle == "" {
		return errors.New("snapshot content driver, source, retention or reference identity mismatch")
	}
	if m.SnapshotHandle != "" && m.SnapshotHandle != content.Status.SnapshotHandle {
		return errors.New("snapshot handle differs from the durable catalog")
	}
	if content.Metadata.UID == "" || (m.ContentUID != "" && m.ContentUID != content.Metadata.UID) {
		return errors.New("snapshot content UID differs from the durable catalog")
	}
	m.ContentUID = content.Metadata.UID
	m.SnapshotHandle = content.Status.SnapshotHandle
	return nil
}

func (c *Controller) localAvailable(ctx context.Context, r Record) error {
	if err := c.checkNamespace(ctx, r); err != nil {
		return err
	}
	for _, m := range r.Members {
		b, err := c.workload(ctx, r.Namespace, nil, "get", "volumesnapshot", m.Snapshot, "-o", "json")
		if err != nil {
			return errors.New("retained VolumeSnapshot is unavailable")
		}
		var snap struct {
			Metadata Metadata `json:"metadata"`
			Status   struct {
				Ready   bool   `json:"readyToUse"`
				Content string `json:"boundVolumeSnapshotContentName"`
			} `json:"status"`
		}
		if err := json.Unmarshal(b, &snap); err != nil {
			return err
		}
		if snap.Metadata.UID != m.SnapshotUID || snap.Metadata.DeletionTimestamp != nil || !snap.Status.Ready || snap.Status.Content != m.Content {
			return errors.New("VolumeSnapshot identity or readiness changed")
		}
		if err := c.resolveSnapshot(ctx, r, &m); err != nil {
			return err
		}
	}
	if len(r.Members) == 0 {
		return errors.New("catalog contains no snapshot members")
	}
	return nil
}
