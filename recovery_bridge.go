package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/backuprepo"
	"github.com/tellmeY18/containers_anywhere/internal/recovery"
	"github.com/tellmeY18/containers_anywhere/internal/recoveryapi"
)

type recoveryBridge struct {
	state      string
	client     *http.Client
	controller *recoveryapi.Controller
	lastLog    time.Time
	restarting bool
}

// The app owns policy reconciliation; the appliance owns only its cold executor.
// Join shutdown so checked hook resumption finishes before closing the VM.
func startRecoveryBridge(parent context.Context, state string, client *http.Client) func() {
	ctx, cancel := context.WithCancel(parent)
	long := recoveryStreamClient(client)
	if real, err := filepath.EvalSymlinks(state); err == nil {
		state = real
	}
	b := &recoveryBridge{state: state, client: long}
	b.controller = &recoveryapi.Controller{
		Run: recoveryKubectl(long), Dir: filepath.Join(state, "recovery-api"), Backend: b,
		HostDestination: b.destination, Export: b.export, RestoreImported: b.restoreImported,
		Prune: b.prune,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer long.CloseIdleConnections()
		for {
			step, stop := context.WithTimeout(ctx, 6*time.Hour)
			b.report(b.reconcileActivity(step))
			b.report(b.controller.Reconcile(step))
			if step.Err() == nil {
				b.report(b.restartIfNeeded(step))
			}
			stop()
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (b *recoveryBridge) report(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	text := err.Error()
	if strings.Contains(text, "doesn't have a resource type") || strings.Contains(text, "could not find the requested resource") {
		return // platform CRDs arrive asynchronously on first boot
	}
	if time.Since(b.lastLog) > time.Minute {
		log.Printf("recovery controller: %v", err)
		b.lastLog = time.Now()
	}
}

func (b *recoveryBridge) Submit(ctx context.Context, op recoveryapi.ClusterOperation) (recoveryapi.OperationStatus, error) {
	if err := checkUpdateStart(b.state); err != nil {
		return recoveryapi.OperationStatus{}, err
	}
	if swap, err := readSwap(b.state); err == nil && swap.Phase == "trying" {
		return recoveryapi.OperationStatus{}, errors.New("finish the root swap trial before requesting recovery")
	} else if err != nil && !os.IsNotExist(err) {
		return recoveryapi.OperationStatus{}, err
	}
	if err := recordRecoveryActivity(b.state, op.UID); err != nil {
		return recoveryapi.OperationStatus{}, err
	}
	var result recovery.Operation
	err := recoveryJSON(ctx, b.client, "POST", "/operations", recovery.Request{
		UID: op.UID, Action: strings.ToLower(op.Action), Scope: op.Scope, PointID: op.RecoveryPointID,
	}, &result)
	if err != nil {
		// An interrupted response may still have queued the operation. Leave
		// the marker until a subsequent status query determines its outcome.
		var refused *recoveryHTTPError
		if errors.As(err, &refused) && refused.Status == http.StatusConflict && !strings.Contains(refused.Message, recovery.ErrBusy.Error()) {
			var existing recovery.Operation
			probeErr := recoveryJSON(ctx, b.client, "GET", "/operations/"+op.UID, nil, &existing)
			var missing *recoveryHTTPError
			if errors.As(probeErr, &missing) && missing.Status == http.StatusNotFound {
				b.report(finishRecoveryActivity(b.state, op.UID))
				return recoveryapi.OperationStatus{Phase: "Failed", Reason: "ExecutorRejected", Message: refused.Message}, nil
			}
		}
		return recoveryapi.OperationStatus{}, err
	}
	return b.operationStatus(result), nil
}

func (b *recoveryBridge) Get(ctx context.Context, uid string) (recoveryapi.OperationStatus, error) {
	if !validRecoveryPathID(uid) {
		return recoveryapi.OperationStatus{}, errors.New("invalid recovery operation ID")
	}
	var op recovery.Operation
	if err := recoveryJSON(ctx, b.client, "GET", "/operations/"+uid, nil, &op); err != nil {
		return recoveryapi.OperationStatus{}, err
	}
	if op.Request.UID != uid {
		return recoveryapi.OperationStatus{}, errors.New("guest returned another recovery operation")
	}
	return b.operationStatus(op), nil
}

func (b *recoveryBridge) operationStatus(op recovery.Operation) recoveryapi.OperationStatus {
	st := recoveryapi.OperationStatus{RecoveryPointID: op.PointID}
	switch op.Phase {
	case "restart-needed":
		st.Phase, st.Reason = "RestartRequired", "ColdCheckpointQueued"
	case "capturing", "restoring", "rolling-back":
		st.Phase, st.Reason = "Running", "GuestRecoveryInProgress"
	case "completed":
		st.Phase, st.Reason = "Succeeded", "ColdOperationComplete"
	case "failed":
		st.Phase, st.Reason, st.Message = "Failed", "GuestRecoveryFailed", op.Error
	default:
		st.Phase, st.Reason, st.Message = "Failed", "InvalidExecutorPhase", "Guest recovery returned an unknown phase"
	}
	if op.Phase == "completed" || op.Phase == "failed" {
		if active, err := readRecoveryActivity(b.state); err == nil && active.UID == op.Request.UID {
			b.report(finishRecoveryActivity(b.state, op.Request.UID))
		}
	}
	return st
}

func (b *recoveryBridge) reconcileActivity(ctx context.Context) error {
	active, err := readRecoveryActivity(b.state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = b.Get(ctx, active.UID)
	if err != nil && strings.Contains(err.Error(), "recovery executor (404)") {
		// The executor proves no request was accepted. Submission is durable
		// and deduplicated; the controller can safely retry its saved request.
		return finishRecoveryActivity(b.state, active.UID)
	}
	return err
}

func (b *recoveryBridge) restartIfNeeded(ctx context.Context) error {
	if b.restarting {
		return nil
	}
	var state struct {
		RestartNeeded bool                 `json:"restartNeeded"`
		Operations    []recovery.Operation `json:"operations"`
	}
	if err := recoveryJSON(ctx, b.client, "GET", "/restart-needed", nil, &state); err != nil {
		return err
	}
	if !state.RestartNeeded {
		return nil
	}
	for _, op := range state.Operations {
		if op.Phase != "restart-needed" {
			return errors.New("guest recovery requires attention; refusing a repeated reboot of an incomplete restore")
		}
		if err := checkUpdateStart(b.state); err != nil {
			return err
		}
		if err := recordRecoveryActivity(b.state, op.Request.UID); err != nil {
			return err
		}
		b.restarting = true
		triggerPower("restart-appliance")
		return nil
	}
	return errors.New("guest requested restart without a durable recovery operation")
}

func (b *recoveryBridge) destination(_ context.Context, id string) (recoveryapi.DestinationStatus, error) {
	condition := recoveryapi.Condition{Type: "Ready", Status: "False", Reason: "NotEnrolled", Message: "Enroll this destination in the launcher before exporting"}
	facts, err := backuprepo.Facts(b.state)
	if err != nil {
		b.report(err)
		condition.Reason, condition.Message = "Unavailable", "Host destination registry is unavailable"
		return recoveryapi.DestinationStatus{Conditions: []recoveryapi.Condition{condition}}, nil
	}
	for _, fact := range facts {
		if fact.ID == id {
			condition.Reason, condition.Message = "Unavailable", "Connect the enrolled backup destination"
			if fact.Ready {
				condition.Status, condition.Reason, condition.Message = "True", "HostDirectoryAvailable", "Enrolled independent host repository is available"
			} else if fact.Available {
				condition.Reason, condition.Message = "StoragePressure", "The enrolled repository is available but its free-space reserve prevents another export"
			}
			break
		}
	}
	return recoveryapi.DestinationStatus{Conditions: []recoveryapi.Condition{condition}}, nil
}

type workloadExportMetadata struct {
	NamespaceUID string                     `json:"namespaceUID"`
	Member       recoveryapi.Member         `json:"member"`
	Config       map[string]json.RawMessage `json:"config,omitempty"`
	Consistency  string                     `json:"consistency"`
}

func durableWorkloadID(uid string) string { return recovery.PointIDForUID("workload-" + uid) }

func (b *recoveryBridge) registerWorkload(ctx context.Context, record recoveryapi.Record) (string, error) {
	request := recovery.RegisterRequest{ID: "workload-" + record.UID}
	if record.Consistency == "QuiescedSequential" {
		request.Consistency = "quiesced-group-hook-contract"
	}
	run := recoveryKubectl(b.client)
	for i, m := range record.Members {
		var content struct {
			Metadata recoveryapi.Metadata `json:"metadata"`
			Spec     struct {
				Driver            string `json:"driver"`
				VolumeSnapshotRef struct {
					UID       string `json:"uid"`
					Namespace string `json:"namespace"`
				} `json:"volumeSnapshotRef"`
			} `json:"spec"`
			Status struct {
				SnapshotHandle string `json:"snapshotHandle"`
				Ready          bool   `json:"readyToUse"`
			} `json:"status"`
		}
		raw, err := run(ctx, nil, "get", "volumesnapshotcontents.snapshot.storage.k8s.io", m.Content, "-o", "json")
		if err != nil {
			return "", err
		}
		if err = json.Unmarshal(raw, &content); err != nil {
			return "", err
		}
		if content.Spec.Driver != "btrfs.csi.local" || content.Spec.VolumeSnapshotRef.UID != m.SnapshotUID || content.Spec.VolumeSnapshotRef.Namespace != record.Namespace || content.Status.SnapshotHandle != m.SnapshotHandle || !content.Status.Ready {
			return "", errors.New("CSI snapshot identity changed before export")
		}
		meta := workloadExportMetadata{NamespaceUID: record.NamespaceUID, Member: m, Consistency: record.Consistency}
		if i == 0 {
			meta.Config = record.Config
		}
		metadata, err := json.Marshal(meta)
		if err != nil {
			return "", err
		}
		request.Members = append(request.Members, recovery.SnapshotMember{
			PVCNamespace: record.Namespace, PVCName: m.PVC, PVCUID: m.UID, PVName: m.VolumeName,
			VolumeHandle: m.VolumeHandle, SnapshotHandle: m.SnapshotHandle,
			SnapshotUID: m.SnapshotUID, SnapshotContentUID: content.Metadata.UID, Driver: "btrfs.csi.local", Metadata: metadata,
		})
	}
	var point recovery.Manifest
	if err := recoveryJSON(ctx, b.client, "POST", "/workload-snapshots", request, &point); err != nil {
		return "", err
	}
	if point.ID != durableWorkloadID(record.UID) || point.Scope != "workload" {
		return "", errors.New("guest registered a different workload point")
	}
	return point.ID, nil
}

func (b *recoveryBridge) export(ctx context.Context, record recoveryapi.Record, destination recoveryapi.Destination) (recoveryapi.ExportStatus, error) {
	fail := func(err error) (recoveryapi.ExportStatus, error) {
		b.report(err)
		return recoveryapi.ExportStatus{Phase: "Failed", Reason: "HostExportFailed", Message: "Independent host export did not complete; inspect launcher logs"}, errors.New("host export failed")
	}
	if destination.Spec.Provider != "HostDirectory" {
		return fail(errors.New("remote backup provider is not implemented"))
	}
	repo, err := backuprepo.Resolve(b.state, destination.Spec.HostDirectoryID)
	if err != nil {
		return fail(err)
	}
	entries, err := repo.List()
	repo.Close()
	if err != nil {
		return fail(err)
	}
	sum := sha256.Sum256([]byte(record.UID + "/" + destination.UID))
	backupID := "backup-" + hex.EncodeToString(sum[:16])
	pointID := record.RecoveryPointID
	scope := "full"
	if record.Namespace != "" {
		pointID, scope = durableWorkloadID(record.UID), "workload"
	} else if record.Operation != nil {
		scope = record.Operation.Scope
	}
	for _, entry := range entries {
		if entry.ID == backupID {
			if entry.PointID != pointID || entry.Scope != scope {
				return fail(errors.New("backup receipt identity mismatch"))
			}
			return recoveryapi.ExportStatus{Phase: "Succeeded", Reason: "EncryptedExportComplete", Receipt: entry.ID, Message: "Independent full export committed; restore validation is separate"}, nil
		}
	}
	if record.Namespace != "" {
		pointID, err = b.registerWorkload(ctx, record)
		if err != nil {
			return fail(err)
		}
	}
	result, err := runRecoveryBackup(ctx, b.client, RecoveryBackupOptions{Operation: "export", State: b.state, DestinationID: destination.Spec.HostDirectoryID, PointID: pointID, BackupID: backupID, Scope: scope})
	if entry, ok := result.(backuprepo.Entry); ok && entry.ID != "" {
		if err != nil {
			b.report(err)
		} // completed export remains valid if retention failed
		return recoveryapi.ExportStatus{Phase: "Succeeded", Reason: "EncryptedExportComplete", Receipt: entry.ID, Message: "Independent full export committed; restore validation is separate"}, nil
	}
	if err == nil {
		err = errors.New("backup export returned no committed receipt")
	}
	return fail(err)
}

func (b *recoveryBridge) prune(ctx context.Context, record recoveryapi.Record) error {
	id := record.RecoveryPointID
	if record.Namespace != "" {
		id = durableWorkloadID(record.UID)
	}
	if id == "" {
		return nil
	}
	if record.Namespace == "" {
		var catalog struct {
			Points []recovery.Manifest `json:"points"`
		}
		if err := recoveryJSON(ctx, b.client, "GET", "/points", nil, &catalog); err != nil {
			return err
		}
		var target *recovery.Manifest
		for i := range catalog.Points {
			if catalog.Points[i].ID == id {
				target = &catalog.Points[i]
			}
		}
		if target == nil {
			return nil
		}
		remaining := 0
		for _, point := range catalog.Points {
			if point.ID != id && point.Scope == target.Scope && point.Appliance == target.Appliance && point.DataFormat == target.DataFormat && point.Datastore.K3s == target.Datastore.K3s {
				remaining++
			}
		}
		if remaining == 0 {
			return errors.New("retention cannot delete the last live complete guest point in this scope")
		}
	}
	err := recoveryJSON(ctx, b.client, "DELETE", "/points/"+id, nil, nil)
	if err != nil && strings.Contains(err.Error(), "recovery executor (404)") {
		return nil
	}
	return err
}

var _ recoveryapi.Backend = (*recoveryBridge)(nil)
