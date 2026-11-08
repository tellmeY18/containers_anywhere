package recoveryapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Controller runs in the app, over its authenticated guest kubectl transport.
// Dir MUST be host state outside data.img. One controller owns each Dir; calls
// to Reconcile on that instance are serialized. Store optionally replaces Dir.
type Controller struct {
	Run             Runner
	Dir             string
	Store           Store
	Backend         Backend
	HostDestination func(context.Context, string) (DestinationStatus, error)
	Export          func(context.Context, Record, Destination) (ExportStatus, error)
	RestoreImported func(context.Context, ImportedRestoreRequest) (ImportedRestoreStatus, error)
	Prune           func(context.Context, Record) error
	CheckCapacity   func(context.Context) error
	Now             func() time.Time
	mu              sync.Mutex
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
func (c *Controller) store() Store {
	if c.Store != nil {
		return c.Store
	}
	return FileStore{Dir: c.Dir}
}
func (c *Controller) run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if c.Run == nil {
		return nil, errors.New("recovery Kubernetes transport is not configured")
	}
	b, err := c.Run(ctx, input, args...)
	if len(b) > 8<<20 {
		return nil, errors.New("recovery Kubernetes response exceeds 8 MiB")
	}
	return b, err
}

var resources = map[string]string{
	"ProtectionPolicy": "protectionpolicies", "RecoveryPoint": "recoverypoints", "RestoreRequest": "restorerequests",
	"ClusterProtectionPolicy": "clusterprotectionpolicies", "ClusterRecoveryPoint": "clusterrecoverypoints", "ClusterRestoreRequest": "clusterrestorerequests", "BackupDestination": "backupdestinations",
}

func (c *Controller) list(ctx context.Context, kind string) ([]Object, error) {
	args := []string{"get", resources[kind] + "." + Group, "-o", "json"}
	if !strings.HasPrefix(kind, "Cluster") && kind != "BackupDestination" {
		args = append(args, "-A")
	}
	b, err := c.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []Object `json:"items"`
	}
	if err = json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	if list.Items == nil || len(list.Items) > 2048 {
		return nil, errors.New("invalid or oversized recovery resource list")
	}
	for i := range list.Items {
		list.Items[i].Kind = kind
	}
	return list.Items, nil
}

func (c *Controller) patchStatus(ctx context.Context, o Object, status any) error {
	_, err := c.patchStatusVersion(ctx, o, status)
	return err
}

func (c *Controller) patchStatusVersion(ctx context.Context, o Object, status any) (string, error) {
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": o.Metadata.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": o.Metadata.ResourceVersion},
		{"op": "add", "path": "/status", "value": status},
	})
	args := []string{"patch", resources[o.Kind] + "." + Group, o.Metadata.Name, "--subresource=status", "--type=json", "-p", string(patch), "-o", "json"}
	if o.Metadata.Namespace != "" {
		args = append(args, "-n", o.Metadata.Namespace)
	}
	b, err := c.run(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	var updated struct {
		Metadata Metadata `json:"metadata"`
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &updated); err != nil {
			return "", err
		}
	}
	if updated.Metadata.ResourceVersion != "" {
		return updated.Metadata.ResourceVersion, nil
	}
	// Non-JSON-aware adapters cannot advance the CAS version: the final write
	// may conflict and is then safely reprojected on the next reconciliation.
	return o.Metadata.ResourceVersion, nil
}

func statusFor(o Object, r Record) Status {
	st := Status{ObservedGeneration: o.Metadata.Generation, Phase: r.Phase, Reason: r.Reason, Message: r.Message, Consistency: r.Consistency, Members: r.Members, RestoredPVCs: r.RestoredPVCs, RecoveryPointID: r.RecoveryPointID, ExportPending: r.ExportPending, ExportReason: r.ExportReason, Export: r.Export, LastSchedule: r.LastSchedule}
	ready := "False"
	if r.Phase == "Succeeded" {
		ready = "True"
	}
	if r.Kind == "ClusterRecoveryPoint" && r.Reason == "GuestJournalRecovered" && r.Phase == "Succeeded" {
		ready = "Unknown"
	}
	st.Conditions = []Condition{{Type: "Ready", Status: ready, Reason: nonempty(r.Reason, r.Phase), Message: r.Message}}
	if r.RetentionEligible {
		st.Conditions = append(st.Conditions, Condition{Type: "RetentionEligible", Status: "True", Reason: "PruneQueued", Message: "Outside keepLast; exact CSI and registered engine identities retire through the durable pruning journal"})
	}
	return st
}

func nonempty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
func terminal(phase string) bool {
	return phase == "Succeeded" || phase == "Failed" || phase == "Pruned"
}

func validateScope(scope string, cluster bool) error {
	if cluster && scope != "system" && scope != "full" {
		return errors.New("cluster scope must be system or full")
	}
	if !cluster && scope != "" {
		return errors.New("namespaced requests cannot specify cluster scope")
	}
	return nil
}
func canonicalHash(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return digest(v), nil
}

// Reconcile first resumes any uncertain quiesce, including orphaned requests,
// then projects the durable catalog and processes bounded schedules/requests.
func (c *Controller) Reconcile(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	all, err := c.store().List()
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range all {
		if r.Kind != "RecoveryPoint" || terminal(r.Phase) || r.Prune != nil {
			continue
		}
		if err := c.recoverInterrupted(ctx, &r); err != nil {
			errs = append(errs, err)
		}
	}
	for _, r := range all {
		if r.Phase == "Pruning" && r.Prune != nil {
			if err := c.pruneRecord(ctx, &r); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		// A queued cold restore may remove its own CR from Kubernetes. Keep
		// observing its host-journaled operation independently of the API.
		if !terminal(r.Phase) && r.Operation != nil && strings.HasPrefix(r.Kind, "Cluster") {
			if err := c.cluster(ctx, &r, *r.Operation); err != nil {
				errs = append(errs, err)
			}
		}
		if r.Phase == "Succeeded" && r.ExportPending {
			if err := c.exportPoint(ctx, &r); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, kind := range []string{"BackupDestination", "RecoveryPoint", "ClusterRecoveryPoint", "RestoreRequest", "ClusterRestoreRequest", "ProtectionPolicy", "ClusterProtectionPolicy"} {
		items, err := c.list(ctx, kind)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, o := range items {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			if o.Metadata.DeletionTimestamp != nil || !safeID.MatchString(o.Metadata.UID) {
				continue
			}
			var err error
			switch kind {
			case "BackupDestination":
				err = c.reconcileDestination(ctx, o)
			case "ProtectionPolicy", "ClusterProtectionPolicy":
				err = c.reconcilePolicy(ctx, o)
			default:
				err = c.reconcileRequest(ctx, o)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s/%s: %w", kind, o.Metadata.Namespace, o.Metadata.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) recoverInterrupted(ctx context.Context, r *Record) error {
	// The prior process may have died after prepare or snapshot publication.
	// Never rerun prepare/capture; resume is the only safe uncertain transition.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	if err := c.resume(cleanup, r); err != nil {
		r.Phase, r.Reason, r.Message = "ResumeFailed", "ResumeCheckFailed", err.Error()
		return errors.Join(err, c.store().Put(*r))
	}
	r.Phase, r.Reason, r.Message = "Failed", "Interrupted", "Capture was interrupted; checked resume completed; partial snapshots retained"
	t := c.now()
	r.CompletedAt = &t
	return c.store().Put(*r)
}

func (c *Controller) reconcileRequest(ctx context.Context, o Object) error {
	newRequest := false
	hash, err := canonicalHash(o.Spec)
	if err != nil {
		return err
	}
	r, err := c.store().Get(o.Metadata.UID)
	if err == nil {
		if r.SpecHash != hash || r.Kind != o.Kind || r.Name != o.Metadata.Name || r.Namespace != o.Metadata.Namespace {
			return errors.New("request UID conflicts with durable journal; refusing replay")
		}
		if terminal(r.Phase) || r.Phase == "ResumeFailed" || r.Phase == "Pruning" {
			st := statusFor(o, r)
			if r.Kind == "RecoveryPoint" && r.Phase == "Succeeded" {
				if err := c.localAvailable(ctx, r); err != nil {
					st.Conditions = []Condition{{Type: "Ready", Status: "False", Reason: "LocalSnapshotUnavailable", Message: "Historical capture succeeded, but current CSI identities are unavailable or differ; an exported backup remains independently usable"}, {Type: "LocalAvailable", Status: "False", Reason: "InventoryChanged", Message: err.Error()}}
				} else {
					st.Conditions = append(st.Conditions, Condition{Type: "LocalAvailable", Status: "True", Reason: "CSIIdentitiesVerified", Message: "Current retained snapshot references match the host catalog"})
				}
			}
			return c.patchStatus(ctx, o, st)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		r = Record{UID: o.Metadata.UID, Name: o.Metadata.Name, Namespace: o.Metadata.Namespace, Kind: o.Kind, SpecHash: hash, Phase: "Prepared", CreatedAt: c.now()}
		if r.Namespace != "" {
			r.NamespaceUID, err = c.namespaceUID(ctx, r.Namespace)
			if err != nil {
				return err
			}
		}
		if historicalStatus(o.Status) {
			return c.quarantineHistorical(ctx, o, &r)
		}
		newRequest = true
	} else {
		return err
	}
	cluster := strings.HasPrefix(o.Kind, "Cluster")
	if cluster && o.Metadata.Namespace != "" || !cluster && o.Metadata.Namespace == "" {
		return errors.New("invalid request scope")
	}
	if newRequest {
		// Persist and publish the start marker BEFORE hooks, guest submission,
		// imported population or PVC creation. A restored CR on a replacement
		// host then carries evidence that replay is unsafe without its journal.
		if err := c.store().Put(r); err != nil {
			return err
		}
		version, err := c.patchStatusVersion(ctx, o, statusFor(o, r))
		if err != nil {
			return err
		}
		o.Metadata.ResourceVersion = version
	}
	if strings.HasSuffix(o.Kind, "RestoreRequest") {
		err = c.restore(ctx, o, &r, cluster)
	} else {
		err = c.point(ctx, o, &r, cluster)
	}
	if err != nil {
		// Transient backend or transport errors leave journaled cluster/restore
		// operations retryable. Workload capture itself handles checked resume.
		return err
	}
	if r.Phase == "Succeeded" && r.ExportPending {
		if err := c.exportPoint(ctx, &r); err != nil {
			return err
		}
	}
	return c.patchStatus(ctx, o, statusFor(o, r))
}

func (c *Controller) reject(r *Record, reason string, err error) error {
	r.Phase, r.Reason, r.Message = "Failed", reason, err.Error()
	t := c.now()
	r.CompletedAt = &t
	return c.store().Put(*r)
}

func (c *Controller) point(ctx context.Context, o Object, r *Record, cluster bool) error {
	var s PointSpec
	if err := json.Unmarshal(o.Spec, &s); err != nil {
		return c.reject(r, "InvalidSpec", err)
	}
	if err := validateScope(s.Scope, cluster); err != nil {
		return c.reject(r, "InvalidScope", err)
	}
	r.Scope = s.Scope
	r.PolicyUID = s.PolicyUID
	if s.PolicyUID != "" {
		policy, err := c.store().Get(s.PolicyUID)
		if err != nil {
			return c.reject(r, "InvalidPolicy", err)
		}
		kind := "ProtectionPolicy"
		if cluster {
			kind = "ClusterProtectionPolicy"
		}
		if policy.Kind != kind || policy.Namespace != r.Namespace {
			return c.reject(r, "InvalidPolicy", errors.New("policy identity belongs to a different namespace or scope"))
		}
	}
	if s.Destination != "" {
		if !dnsName(s.Destination) {
			return c.reject(r, "InvalidDestination", errors.New("invalid destination name"))
		}
		d, err := c.destination(ctx, s.Destination, r.Namespace)
		if err != nil {
			return err
		}
		if r.DestinationUID != "" && r.DestinationUID != d.UID {
			return c.reject(r, "DestinationChanged", errors.New("destination was replaced"))
		}
		r.Destination, r.DestinationUID = d.Name, d.UID
		// Local capture remains usable, but no export is reported as completed.
		r.ExportPending, r.ExportReason = true, "AwaitingLocalCapture"
	}
	if cluster {
		if s.Capture != nil {
			return c.reject(r, "InvalidSpec", errors.New("cluster capture uses the guest engine"))
		}
		return c.cluster(ctx, r, ClusterOperation{UID: r.UID, Scope: s.Scope, Action: "Capture", Destination: s.Destination})
	}
	if s.Capture == nil {
		return c.reject(r, "InvalidSpec", errors.New("capture is required"))
	}
	if err := s.Capture.Validate(); err != nil {
		return c.reject(r, "InvalidSpec", err)
	}
	if c.CheckCapacity != nil {
		if err := c.CheckCapacity(ctx); err != nil {
			return c.reject(r, "StoragePressure", errors.New("capture requires healthy host and guest storage reserves; retry with a new request UID after capacity recovers"))
		}
	}
	all, err := c.store().List()
	if err != nil {
		return err
	}
	for _, prior := range all {
		if prior.UID == r.UID || prior.Namespace != r.Namespace || prior.NamespaceUID != r.NamespaceUID {
			continue
		}
		if prior.ResumeUncertain {
			return c.reject(r, "ResumeBlocked", errors.New("historical capture lacks its host hook journal; operator resume checks must resolve the quarantined record first"))
		}
		for _, attempt := range prior.Hooks {
			if !attempt.Resumed {
				return c.reject(r, "ResumeBlocked", errors.New("a prior hook in this namespace has not completed checked resume"))
			}
		}
	}
	// A class with Delete would lose data during namespace removal.
	b, err := c.run(ctx, nil, "get", "volumesnapshotclass", SnapshotClass, "-o", "json")
	if err != nil {
		return err
	}
	var class struct {
		Driver         string `json:"driver"`
		DeletionPolicy string `json:"deletionPolicy"`
	}
	if err := json.Unmarshal(b, &class); err != nil {
		return err
	}
	if class.Driver != "btrfs.csi.local" || class.DeletionPolicy != "Retain" {
		return c.reject(r, "UnsafeSnapshotClass", errors.New("recovery class must use btrfs.csi.local and Retain"))
	}
	r.Members, err = c.selectMembers(ctx, r.Namespace, *s.Capture)
	if err != nil {
		return c.reject(r, "SelectionFailed", err)
	}
	if err := c.resolveVolumes(ctx, r); err != nil {
		return c.reject(r, "VolumeIdentityFailed", err)
	}
	for i := range r.Members {
		r.Members[i].Snapshot = generatedName("rp-", r.UID, r.Members[i].PVC)
	}
	r.Consistency = "CrashConsistentPerPVC"
	if s.Capture.Consistency == "QuiescedGroup" {
		r.Consistency = "QuiescedSequential"
	}
	if err := c.store().Put(*r); err != nil {
		return err
	}
	captureErr := c.capture(ctx, r, *s.Capture)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	resumeErr := c.resume(cleanup, r)
	if resumeErr != nil {
		r.Phase, r.Reason, r.Message = "ResumeFailed", "ResumeCheckFailed", errors.Join(captureErr, resumeErr).Error()
		return c.store().Put(*r)
	}
	if captureErr != nil {
		return c.reject(r, "CaptureFailed", captureErr)
	}
	r.Phase, r.Reason, r.Message = "Succeeded", "LocalCaptureComplete", "Sequential CSI snapshots; no atomic group capture or PITR guarantee"
	t := c.now()
	r.CompletedAt = &t
	return c.store().Put(*r)
}

func (c *Controller) cluster(ctx context.Context, r *Record, op ClusterOperation) error {
	if err := validateScope(op.Scope, true); err != nil {
		return err
	}
	if r.Operation != nil && *r.Operation != op {
		return errors.New("cluster operation differs from durable request")
	}
	r.Operation = &op
	if c.Backend == nil {
		r.Phase, r.Reason, r.Message = "Pending", "BackendUnavailable", "Cluster recovery backend is not configured"
		return c.store().Put(*r)
	}
	if op.Action == "Capture" && !r.BackendSubmitted && !r.BackendAttempted && c.CheckCapacity != nil {
		if err := c.CheckCapacity(ctx); err != nil {
			return c.reject(r, "StoragePressure", errors.New("capture requires healthy host and guest storage reserves; retry with a new request UID after capacity recovers"))
		}
	}
	if !r.BackendSubmitted {
		r.BackendAttempted = true
	}
	if err := c.store().Put(*r); err != nil {
		return err
	}
	var st OperationStatus
	var err error
	if r.BackendSubmitted {
		st, err = c.Backend.Get(ctx, r.UID)
	} else {
		st, err = c.Backend.Submit(ctx, op)
	}
	if err != nil {
		return err
	}
	switch st.Phase {
	case "Pending", "Running", "RestartRequired", "Succeeded", "Failed":
	default:
		return errors.New("backend returned an invalid phase")
	}
	r.BackendSubmitted = true
	r.Phase, r.Reason, r.Message, r.RecoveryPointID = st.Phase, st.Reason, st.Message, st.RecoveryPointID
	if terminal(r.Phase) {
		t := c.now()
		r.CompletedAt = &t
	}
	if op.Action == "Capture" && st.Phase == "Succeeded" && st.RecoveryPointID == "" {
		return errors.New("backend success has no recovery point ID")
	}
	return c.store().Put(*r)
}

func (c *Controller) restore(ctx context.Context, o Object, r *Record, cluster bool) error {
	var s RestoreSpec
	if err := json.Unmarshal(o.Spec, &s); err != nil {
		return c.reject(r, "InvalidSpec", err)
	}
	if err := validateScope(s.Scope, cluster); err != nil {
		return c.reject(r, "InvalidScope", err)
	}
	r.Scope = s.Scope
	if cluster && s.DurablePointID != "" {
		if s.RecoveryPointID != "" {
			return c.reject(r, "InvalidSpec", errors.New("choose only one direct source ID"))
		}
		s.RecoveryPointID, s.DurablePointID = s.DurablePointID, ""
	}
	if s.RecoveryPointID != "" {
		if !cluster || s.RecoveryPoint != "" || s.RecoveryPointUID != "" || s.DurablePointID != "" || len(s.PVCNames) != 0 || !safeID.MatchString(s.RecoveryPointID) {
			return c.reject(r, "InvalidSpec", errors.New("recoveryPointID requires a cluster-only direct source, without other source references or PVC mappings"))
		}
		all, err := c.store().List()
		if err != nil {
			return err
		}
		for _, point := range all {
			if point.Kind != "ClusterRecoveryPoint" || point.RecoveryPointID != s.RecoveryPointID {
				continue
			}
			if point.Scope != s.Scope {
				return c.reject(r, "InvalidSource", errors.New("source point has different scope"))
			}
			// A direct imported source can reuse an archived immutable point ID.
			// Its former local catalog tombstone does not invalidate the archive;
			// the backend must verify and protect the currently imported manifest.
			if point.Phase == "Pruned" {
				continue
			}
			if point.Phase != "Succeeded" {
				return c.reject(r, "InvalidSource", errors.New("source point is retiring or incomplete"))
			}
			if err := c.protect(&point, r.UID); err != nil {
				return err
			}
		}
		// Imported engine points need not have a Kubernetes capture CR. Backend
		// validates the immutable manifest scope and pins the point before queuing.
		return c.cluster(ctx, r, ClusterOperation{UID: r.UID, Action: "Restore", Scope: s.Scope, RecoveryPointID: s.RecoveryPointID})
	}
	if s.DurablePointID != "" {
		if cluster || s.RecoveryPoint != "" || s.RecoveryPointUID != "" {
			return c.reject(r, "InvalidSpec", errors.New("durablePointID is namespaced and mutually exclusive with a local recoveryPoint reference"))
		}
		return c.restoreImported(ctx, r, s)
	}
	if !dnsName(s.RecoveryPoint) || !safeID.MatchString(s.RecoveryPointUID) {
		return c.reject(r, "InvalidSpec", errors.New("recoveryPoint name and UID required"))
	}
	point, err := c.store().Get(s.RecoveryPointUID)
	if err != nil {
		return err
	}
	kind := "RecoveryPoint"
	if cluster {
		kind = "ClusterRecoveryPoint"
	}
	if point.Kind != kind || point.Name != s.RecoveryPoint || point.Namespace != r.Namespace || point.Phase != "Succeeded" || point.Scope != s.Scope || (!cluster && point.NamespaceUID != r.NamespaceUID) {
		return c.reject(r, "InvalidSource", errors.New("source must be a successful point in this exact scope and namespace identity"))
	}
	// Durable protection precedes any restore submission or new PVC creation.
	if err := c.protect(&point, r.UID); err != nil {
		return err
	}
	if cluster {
		if len(s.PVCNames) != 0 {
			return c.reject(r, "InvalidSpec", errors.New("cluster restore does not accept PVC mappings"))
		}
		return c.cluster(ctx, r, ClusterOperation{UID: r.UID, Scope: s.Scope, Action: "Restore", RecoveryPointID: point.RecoveryPointID})
	}
	if err := c.checkNamespace(ctx, *r); err != nil {
		return err
	}
	if len(s.PVCNames) != len(point.Members) {
		return c.reject(r, "InvalidMapping", errors.New("pvcNames must map every captured PVC exactly once"))
	}
	seen := map[string]bool{}
	for _, m := range point.Members {
		name := s.PVCNames[m.PVC]
		if !dnsName(name) || name == m.PVC || seen[name] {
			return c.reject(r, "InvalidMapping", errors.New("restore requires unique new PVC names for every member"))
		}
		seen[name] = true
	}
	if r.RestoredPVCs == nil {
		r.RestoredPVCs = map[string]string{}
	}
	if err := c.store().Put(*r); err != nil {
		return err
	}
	for _, m := range point.Members {
		if !m.Ready || m.Content == "" || m.SnapshotUID == "" {
			return c.reject(r, "IncompleteSource", errors.New("source snapshot is incomplete"))
		}
		b, err := c.workload(ctx, r.Namespace, nil, "get", "volumesnapshot", m.Snapshot, "-o", "json")
		if err != nil {
			return c.reject(r, "SourceUnavailable", errors.New("local CSI snapshot unavailable; use a verified exported/imported point after inventory rewind"))
		}
		var snap struct {
			Metadata Metadata `json:"metadata"`
			Status   struct {
				Ready   bool   `json:"readyToUse"`
				Content string `json:"boundVolumeSnapshotContentName"`
			} `json:"status"`
		}
		if err = json.Unmarshal(b, &snap); err != nil {
			return err
		}
		if snap.Metadata.UID != m.SnapshotUID || !snap.Status.Ready || snap.Status.Content != m.Content {
			return c.reject(r, "SourceChanged", errors.New("retained snapshot identity/readiness changed; administrator import required"))
		}
		if err := c.resolveSnapshot(ctx, point, &m); err != nil {
			return c.reject(r, "SourceChanged", err)
		}
		if err := c.checkNamespace(ctx, *r); err != nil {
			return err
		}
		name := s.PVCNames[m.PVC]
		spec := map[string]any{"storageClassName": m.StorageClass, "accessModes": m.AccessModes, "resources": map[string]any{"requests": map[string]string{"storage": m.Storage}}, "dataSource": map[string]string{"apiGroup": "snapshot.storage.k8s.io", "kind": "VolumeSnapshot", "name": m.Snapshot}}
		if m.VolumeMode != "" {
			spec["volumeMode"] = m.VolumeMode
		}
		obj := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": name, "namespace": r.Namespace, "labels": map[string]string{Group + "/request-uid": r.UID}}, "spec": spec}
		if err := c.createExact(ctx, r.Namespace, "persistentvolumeclaim", name, obj, r.UID); err != nil {
			return err
		}
		r.RestoredPVCs[m.PVC] = name
		if err := c.store().Put(*r); err != nil {
			return err
		}
	}
	r.Phase, r.Reason, r.Message = "Succeeded", "NewPVCsCreated", "New PVC declarations created; WaitForFirstConsumer may delay binding. Validate data and explicitly cut over; captured ConfigMaps are catalog-only."
	t := c.now()
	r.CompletedAt = &t
	return c.store().Put(*r)
}

func (c *Controller) reconcilePolicy(ctx context.Context, o Object) error {
	var s PolicySpec
	if err := json.Unmarshal(o.Spec, &s); err != nil {
		return err
	}
	cluster := o.Kind == "ClusterProtectionPolicy"
	if err := s.Validate(cluster); err != nil {
		return c.patchStatus(ctx, o, Status{ObservedGeneration: o.Metadata.Generation, Phase: "Failed", Reason: "InvalidSpec", Message: err.Error()})
	}
	if s.Destination != "" {
		if _, err := c.destination(ctx, s.Destination, o.Metadata.Namespace); err != nil {
			return c.patchStatus(ctx, o, Status{ObservedGeneration: o.Metadata.Generation, Phase: "Failed", Reason: "DestinationDenied", Message: err.Error()})
		}
	}
	r, err := c.store().Get(o.Metadata.UID)
	if errors.Is(err, os.ErrNotExist) {
		r = Record{UID: o.Metadata.UID, Kind: o.Kind, Name: o.Metadata.Name, Namespace: o.Metadata.Namespace, CreatedAt: c.now(), Phase: "Active"}
	} else if err != nil {
		return err
	}
	if r.Kind != o.Kind || r.Namespace != o.Metadata.Namespace || r.Name != o.Metadata.Name {
		return errors.New("policy journal identity mismatch")
	}
	r.Reason, r.Message = "", ""
	if s.Suspend {
		r.Phase = "Suspended"
	} else {
		r.Phase = "Active"
		next := r.PendingSchedule
		if next == nil {
			schedule, _ := ParseSchedule(s.Schedule)
			after := o.Metadata.CreationTimestamp
			if r.LastSchedule != nil {
				after = *r.LastSchedule
			}
			if slot, ok := schedule.Due(c.now(), after); ok {
				next = &slot
			}
		}
		if next != nil {
			reason, message, err := c.scheduleGate(ctx, &r, s.KeepLast)
			if err != nil {
				return err
			}
			if reason != "" {
				r.Phase, r.Reason, r.Message = "Holding", reason, message
			} else if r.PendingSchedule == nil {
				r.PendingSchedule = next
				if err := c.store().Put(r); err != nil {
					return err
				}
			}
		}
		if r.PendingSchedule != nil && r.Phase != "Holding" {
			kind := "RecoveryPoint"
			if cluster {
				kind = "ClusterRecoveryPoint"
			}
			name := generatedName("scheduled-", o.Metadata.UID, r.PendingSchedule.Format(time.RFC3339))
			spec := PointSpec{Scope: s.Scope, Capture: s.Capture, PolicyUID: o.Metadata.UID, ScheduledAt: r.PendingSchedule, Destination: s.Destination}
			obj := map[string]any{"apiVersion": Group + "/" + Version, "kind": kind, "metadata": map[string]any{"name": name, "namespace": o.Metadata.Namespace}, "spec": spec}
			args := []string{"get", resources[kind] + "." + Group, name, "--ignore-not-found", "-o", "json"}
			if !cluster {
				args = append(args, "-n", o.Metadata.Namespace)
			}
			b, err := c.run(ctx, nil, args...)
			if err != nil {
				return err
			}
			if len(strings.TrimSpace(string(b))) == 0 {
				input, _ := json.Marshal(obj)
				if _, err := c.run(ctx, input, "create", "-f", "-"); err != nil {
					return err
				}
			} else {
				var existing Object
				if err := json.Unmarshal(b, &existing); err != nil {
					return err
				}
				var ps PointSpec
				if err := json.Unmarshal(existing.Spec, &ps); err != nil {
					return err
				}
				if ps.PolicyUID != o.Metadata.UID || ps.ScheduledAt == nil || !ps.ScheduledAt.Equal(*r.PendingSchedule) {
					return errors.New("scheduled recovery point name collision")
				}
			}
			r.AwaitingCapture = name
			r.LastSchedule, r.PendingSchedule = r.PendingSchedule, nil
		}
	}
	if err := c.store().Put(r); err != nil {
		return err
	}
	all, err := c.store().List()
	if err != nil {
		return err
	}
	candidates := map[string]bool{}
	var available []Record
	for _, point := range all {
		if point.PolicyUID == o.Metadata.UID && point.Phase == "Succeeded" && c.retentionAvailable(ctx, point) {
			available = append(available, point)
		}
	}
	for _, uid := range RetentionCandidates(available, o.Metadata.UID, s.KeepLast) {
		candidates[uid] = true
	}
	var pruneErrs []error
	for _, point := range all {
		if point.PolicyUID == o.Metadata.UID && point.RetentionEligible != candidates[point.UID] {
			point.RetentionEligible = candidates[point.UID]
			if err := c.store().Put(point); err != nil {
				return err
			}
		}
		if candidates[point.UID] {
			if err := c.pruneRecord(ctx, &point); err != nil {
				pruneErrs = append(pruneErrs, err)
			}
		}
	}
	st := statusFor(o, r)
	st.Conditions = []Condition{{Type: "Retention", Status: "True", Reason: "PruningEnabled", Message: "Eligible sets retire through journaled CSI and engine deletion; physical space reclamation is eventual"}}
	if len(pruneErrs) > 0 {
		st.Conditions = []Condition{{Type: "Retention", Status: "False", Reason: "PruneBlocked", Message: "An eligible set could not be safely pruned; catalog and deletion journal retained"}}
	}
	if r.Phase == "Holding" {
		st.Conditions = append(st.Conditions, Condition{Type: "Capture", Status: "False", Reason: r.Reason, Message: r.Message})
	}
	return errors.Join(c.patchStatus(ctx, o, st), errors.Join(pruneErrs...))
}

func (c *Controller) protect(point *Record, requestUID string) error {
	for _, uid := range point.ProtectedBy {
		if uid == requestUID {
			return nil
		}
	}
	point.ProtectedBy = append(point.ProtectedBy, requestUID)
	point.RetentionEligible = false
	return c.store().Put(*point)
}

// Destination readiness reports enrollment/provider capability, never export
// success. Remote credentials are references only and are not read by this code.
func (c *Controller) Destination(ctx context.Context, s DestinationSpec) (DestinationStatus, error) {
	condition := func(status, reason, message string) DestinationStatus {
		return DestinationStatus{Conditions: []Condition{{Type: "Ready", Status: status, Reason: reason, Message: message}}}
	}
	switch s.Provider {
	case "HostDirectory":
		if !safeID.MatchString(s.HostDirectoryID) || s.Endpoint != "" || s.Bucket != "" || s.Prefix != "" || s.CredentialsSecretRef != nil {
			return condition("False", "InvalidSpec", "HostDirectory requires only an opaque hostDirectoryID"), nil
		}
		if c.HostDestination == nil {
			return condition("False", "HostUnavailable", "Host enrollment callback is unavailable"), nil
		}
		st, err := c.HostDestination(ctx, s.HostDirectoryID)
		if err != nil {
			return condition("False", "HostUnavailable", "Host destination enrollment is unavailable"), nil
		}
		return st, nil
	case "S3Compatible", "AzureBlob", "GCS", "SFTP", "RepositoryService":
		if s.HostDirectoryID != "" {
			return condition("False", "InvalidSpec", "Remote providers forbid hostDirectoryID"), nil
		}
		return condition("False", "NotImplemented", "Remote provider execution is not implemented"), nil
	default:
		return condition("False", "InvalidProvider", "Unknown backup destination provider"), nil
	}
}

func (c *Controller) reconcileDestination(ctx context.Context, o Object) error {
	var s DestinationSpec
	if err := json.Unmarshal(o.Spec, &s); err != nil {
		return err
	}
	st, err := c.Destination(ctx, s)
	if err != nil {
		return err
	}
	return c.patchStatus(ctx, o, st)
}
