package recoveryapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type pruneContent struct {
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
		SnapshotHandle string `json:"snapshotHandle"`
		Ready          bool   `json:"readyToUse"`
	} `json:"status"`
}

// retentionAvailable checks real retained content before it can displace an
// older successful set. A newer host catalog entry alone is not evidence that
// its snapshots survived a guest rewind. Retained orphan content is sufficient
// even when its original namespace no longer exists.
func (c *Controller) retentionAvailable(ctx context.Context, r Record) bool {
	if r.Kind != "RecoveryPoint" {
		return true
	} // guest callback verifies its live catalog
	if len(r.Members) == 0 {
		return false
	}
	for _, m := range r.Members {
		content, err := c.pruneContent(ctx, r, m, false)
		if err != nil || content == nil || !content.Status.Ready {
			return false
		}
	}
	return true
}

func (c *Controller) pruneWitness(ctx context.Context, r Record) error {
	if r.Kind != "RecoveryPoint" {
		return nil
	} // parent checks actual cluster catalog
	all, err := c.store().List()
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.UID == r.UID || other.Kind != r.Kind || other.PolicyUID != r.PolicyUID || other.NamespaceUID != r.NamespaceUID || other.Scope != r.Scope || other.Phase != "Succeeded" || other.CompletedAt == nil || r.CompletedAt == nil {
			continue
		}
		newer := other.CompletedAt.After(*r.CompletedAt) || (other.CompletedAt.Equal(*r.CompletedAt) && other.UID < r.UID)
		if !newer {
			continue
		}
		disjoint := true
		for _, a := range r.Members {
			for _, b := range other.Members {
				if a.ContentUID == b.ContentUID || a.SnapshotHandle == b.SnapshotHandle {
					disjoint = false
				}
			}
		}
		if disjoint && c.retentionAvailable(ctx, other) {
			return nil
		}
	}
	return errors.New("no newer live complete independent set; retirement paused after inventory change")
}

// PruneEligible is an administrator-only host entry point for an independently
// cataloged orphan. It cannot force retention, remove dependencies, or prune the
// last live complete set. Never expose it through namespaced workload authority.
func (c *Controller) PruneEligible(ctx context.Context, uid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.store().Get(uid)
	if err != nil {
		return err
	}
	if r.Prune != nil {
		return c.pruneRecord(ctx, &r)
	}
	if !r.RetentionEligible {
		return errors.New("point is not eligible under its recorded retention policy")
	}
	all, err := c.store().List()
	if err != nil {
		return err
	}
	var available []Record
	for _, point := range all {
		if point.PolicyUID == r.PolicyUID && point.Phase == "Succeeded" && c.retentionAvailable(ctx, point) {
			available = append(available, point)
		}
	}
	for _, candidate := range RetentionCandidates(available, r.PolicyUID, 1) {
		if candidate == uid {
			return c.pruneRecord(ctx, &r)
		}
	}
	return errors.New("point is the last live complete set or has protected dependencies")
}

func (c *Controller) pruneContent(ctx context.Context, r Record, m Member, armed bool) (*pruneContent, error) {
	if m.ContentUID == "" || m.SnapshotUID == "" || m.VolumeHandle == "" || m.SnapshotHandle == "" || !m.Ready {
		return nil, errors.New("incomplete immutable CSI catalog identity")
	}
	b, err := c.run(ctx, nil, "get", "volumesnapshotcontent", m.Content, "--ignore-not-found", "-o", "json")
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var content pruneContent
	if err := json.Unmarshal(b, &content); err != nil {
		return nil, err
	}
	ref := content.Spec.VolumeSnapshotRef
	if content.Metadata.UID != m.ContentUID || content.Metadata.ResourceVersion == "" || ref.UID != m.SnapshotUID || ref.Name != m.Snapshot || ref.Namespace != r.Namespace || content.Spec.Driver != "btrfs.csi.local" || content.Spec.Source.VolumeHandle != m.VolumeHandle || content.Status.SnapshotHandle != m.SnapshotHandle {
		return nil, errors.New("retained content identity changed; refusing pruning")
	}
	if content.Spec.DeletionPolicy != "Retain" && !(armed && content.Spec.DeletionPolicy == "Delete") {
		return nil, errors.New("unowned snapshot content deletion-policy transition")
	}
	return &content, nil
}

// A deleted/recreated namespace is not permission to touch its new objects.
// Independent retained content is still addressable by its original exact UID.
func (c *Controller) pruneNamespacePresent(ctx context.Context, r Record) (bool, error) {
	b, err := c.run(ctx, nil, "get", "namespace", r.Namespace, "--ignore-not-found", "-o", "json")
	if err != nil {
		return false, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return false, nil
	}
	var ns struct {
		Metadata Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(b, &ns); err != nil {
		return false, err
	}
	return ns.Metadata.UID == r.NamespaceUID, nil
}

func (c *Controller) pruneSnapshot(ctx context.Context, r Record, m Member) (*Metadata, error) {
	present, err := c.pruneNamespacePresent(ctx, r)
	if err != nil || !present {
		return nil, err
	}
	b, err := c.workload(ctx, r.Namespace, nil, "get", "volumesnapshot", m.Snapshot, "--ignore-not-found", "-o", "json")
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var snap struct {
		Metadata Metadata `json:"metadata"`
		Spec     struct {
			Class  string `json:"volumeSnapshotClassName"`
			Source struct {
				PVC string `json:"persistentVolumeClaimName"`
			} `json:"source"`
		} `json:"spec"`
		Status struct {
			Content string `json:"boundVolumeSnapshotContentName"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	if snap.Metadata.UID != m.SnapshotUID || snap.Metadata.ResourceVersion == "" || snap.Metadata.Labels[Group+"/request-uid"] != r.UID || snap.Spec.Class != SnapshotClass || snap.Spec.Source.PVC != m.PVC || snap.Status.Content != m.Content {
		return nil, errors.New("VolumeSnapshot identity changed; refusing pruning")
	}
	return &snap.Metadata, nil
}

// User-created snapshot restores are conservatively protected as well. CSI's
// own snapshot protection finalizers close the API-side deletion race; they are
// never stripped. The host catalog protects controller-created restore sources.
func (c *Controller) checkSnapshotConsumers(ctx context.Context, r Record) error {
	present, err := c.pruneNamespacePresent(ctx, r)
	if err != nil || !present {
		return err
	}
	b, err := c.workload(ctx, r.Namespace, nil, "get", "persistentvolumeclaims", "-o", "json")
	if err != nil {
		return err
	}
	var list struct {
		Items []struct {
			Spec struct {
				DataSource *struct {
					Name     string `json:"name"`
					Kind     string `json:"kind"`
					APIGroup string `json:"apiGroup"`
				} `json:"dataSource"`
				DataSourceRef *struct {
					Name     string `json:"name"`
					Kind     string `json:"kind"`
					APIGroup string `json:"apiGroup"`
				} `json:"dataSourceRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	if list.Items == nil {
		return errors.New("PVC consumer list is incomplete")
	}
	for _, claim := range list.Items {
		for _, m := range r.Members {
			if ref := claim.Spec.DataSource; ref != nil && ref.Kind == "VolumeSnapshot" && ref.APIGroup == "snapshot.storage.k8s.io" && ref.Name == m.Snapshot {
				return errors.New("snapshot is referenced by a restore PVC")
			}
			if ref := claim.Spec.DataSourceRef; ref != nil && ref.Kind == "VolumeSnapshot" && ref.APIGroup == "snapshot.storage.k8s.io" && ref.Name == m.Snapshot {
				return errors.New("snapshot is referenced by a restore PVC")
			}
		}
	}
	return nil
}

// DeleteOptions are sent as the raw DELETE body so Kubernetes checks BOTH UID
// and resourceVersion at mutation time. Never use an unconditioned name delete.
func (c *Controller) deleteExact(ctx context.Context, namespace, path string, meta Metadata) error {
	body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": meta.UID, "resourceVersion": meta.ResourceVersion}})
	args := []string{"delete", "--raw=" + path, "-f", "-"}
	var err error
	if namespace != "" {
		_, err = c.workload(ctx, namespace, body, args...)
	} else {
		_, err = c.run(ctx, body, args...)
	}
	return err
}

// pruneRecord is a resumable retirement transaction. The parent callback only
// prunes registered guest points (workload-<UID> for exports); this code owns CSI
// deletion. Its callback must be idempotent and reject protected engine points.
func (c *Controller) pruneRecord(ctx context.Context, r *Record) (err error) {
	defer func() {
		if err != nil && r.Prune != nil && r.Phase != "Pruned" {
			r.Reason, r.Message = "PruneBlocked", "Deletion paused because an identity, dependency, transport or backend check failed; journal retained for retry"
			err = errors.Join(err, c.store().Put(*r))
		}
	}()
	if r.Phase == "Pruned" {
		return nil
	}
	if r.Prune != nil && r.Prune.Members == nil {
		r.Prune.Members = map[string]PruneMember{}
	}
	if r.ExportPending || len(r.ProtectedBy) > 0 {
		return errors.New("recovery point has export or restore dependencies")
	}
	if c.Prune == nil {
		return errors.New("guest point prune adapter is not configured")
	}
	if r.Prune == nil {
		if r.Phase != "Succeeded" || !r.RetentionEligible || r.CompletedAt == nil {
			return errors.New("only eligible complete recovery points may retire")
		}
		if r.Kind == "RecoveryPoint" {
			if len(r.Members) == 0 {
				return errors.New("complete workload point has no members")
			}
			if err := c.checkSnapshotConsumers(ctx, *r); err != nil {
				return err
			}
			for _, m := range r.Members {
				if _, err := c.pruneContent(ctx, *r, m, false); err != nil {
					return err
				}
				if _, err := c.pruneSnapshot(ctx, *r, m); err != nil {
					return err
				}
			}
		} else if r.Kind != "ClusterRecoveryPoint" {
			return errors.New("only recovery points can be pruned")
		}
		r.Prune = &PruneState{StartedAt: c.now(), Members: map[string]PruneMember{}}
		r.Phase, r.Reason, r.Message = "Pruning", "Retiring", "Retirement journal committed; new restores from this local point are refused"
		if err := c.store().Put(*r); err != nil {
			return err
		}
	}
	if !r.Prune.BackendDone {
		if err := c.pruneWitness(ctx, *r); err != nil {
			return err
		}
		if err := c.Prune(ctx, *r); err != nil {
			return err
		}
		r.Prune.BackendDone = true
		if err := c.store().Put(*r); err != nil {
			return err
		}
	}
	for _, m := range r.Members {
		state := r.Prune.Members[m.Content]
		if state.Complete {
			continue
		}
		if err := c.checkSnapshotConsumers(ctx, *r); err != nil {
			return err
		}
		content, err := c.pruneContent(ctx, *r, m, state.DeleteArmed)
		if err != nil {
			return err
		}
		snap, err := c.pruneSnapshot(ctx, *r, m)
		if err != nil {
			return err
		}
		if snap != nil {
			// The retained content stays Retain until its exact VolumeSnapshot
			// is fully removed, including any CSI restore-protection finalizer.
			if snap.DeletionTimestamp == nil {
				if err := c.pruneWitness(ctx, *r); err != nil {
					return err
				}
				path := "/apis/snapshot.storage.k8s.io/v1/namespaces/" + r.Namespace + "/volumesnapshots/" + m.Snapshot
				if err := c.deleteExact(ctx, r.Namespace, path, *snap); err != nil {
					return err
				}
			}
			r.Reason, r.Message = "WaitingForSnapshotDeletion", "Waiting for exact VolumeSnapshot deletion and protection finalizers"
			return c.store().Put(*r)
		}
		if content == nil {
			state.Complete = true
			r.Prune.Members[m.Content] = state
			if err := c.store().Put(*r); err != nil {
				return err
			}
			continue
		}
		if content.Spec.DeletionPolicy == "Retain" {
			if err := c.pruneWitness(ctx, *r); err != nil {
				return err
			}
			// Arm durably BEFORE the patch. A crash between patch and the next
			// journal write can then safely recognize our own Delete transition.
			state.DeleteArmed = true
			r.Prune.Members[m.Content] = state
			if err := c.store().Put(*r); err != nil {
				return err
			}
			patch, _ := json.Marshal([]map[string]any{
				{"op": "test", "path": "/metadata/uid", "value": m.ContentUID},
				{"op": "test", "path": "/metadata/resourceVersion", "value": content.Metadata.ResourceVersion},
				{"op": "test", "path": "/spec/driver", "value": "btrfs.csi.local"},
				{"op": "test", "path": "/spec/source/volumeHandle", "value": m.VolumeHandle},
				{"op": "test", "path": "/spec/volumeSnapshotRef/uid", "value": m.SnapshotUID},
				{"op": "test", "path": "/status/snapshotHandle", "value": m.SnapshotHandle},
				{"op": "test", "path": "/spec/deletionPolicy", "value": "Retain"},
				{"op": "replace", "path": "/spec/deletionPolicy", "value": "Delete"},
			})
			if _, err := c.run(ctx, nil, "patch", "volumesnapshotcontent", m.Content, "--type=json", "-p", string(patch)); err != nil {
				return err
			}
			// Refresh resourceVersion before sending a preconditioned DELETE.
			content, err = c.pruneContent(ctx, *r, m, true)
			if err != nil {
				return err
			}
			if content == nil {
				return nil
			}
		}
		if content.Metadata.DeletionTimestamp == nil {
			if err := c.pruneWitness(ctx, *r); err != nil {
				return err
			}
			if err := c.deleteExact(ctx, "", "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/"+m.Content, content.Metadata); err != nil {
				return err
			}
		}
		r.Reason, r.Message = "WaitingForCSIReclaim", "CSI content deletion requested; waiting for driver finalization, with no forced finalizer removal"
		return c.store().Put(*r)
	}
	r.Phase, r.Reason, r.Message = "Pruned", "Pruned", "Exact CSI resources and registered guest point retired; filesystem space reclamation may be eventual"
	r.RetentionEligible = false
	if err := c.store().Put(*r); err != nil {
		return fmt.Errorf("persist retirement tombstone: %w", err)
	}
	return nil
}
