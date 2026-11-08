package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/tellmeY18/containers_anywhere/internal/recovery"
	"github.com/tellmeY18/containers_anywhere/internal/recoveryapi"
)

const recoveryOwnerLabel = recoveryapi.Group + "/restore-uid"
const recoveryFenceLabel = recoveryapi.Group + "/restoring"

type importedTarget = recovery.PopulateTarget

func recoveryDNSName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func recoveryNotFound(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found"))
}

func (b *recoveryBridge) restoreImported(ctx context.Context, req recoveryapi.ImportedRestoreRequest) (recoveryapi.ImportedRestoreStatus, error) {
	fail := func(err error) (recoveryapi.ImportedRestoreStatus, error) {
		b.report(err)
		return recoveryapi.ImportedRestoreStatus{Phase: "Failed", Reason: "ImportedRestoreRefused", Message: err.Error()}, nil
	}
	wait := func(err error) (recoveryapi.ImportedRestoreStatus, error) {
		b.report(err)
		return recoveryapi.ImportedRestoreStatus{Phase: "Pending", Reason: "AwaitingExecutor", Message: "Waiting for the recovery executor and Kubernetes API"}, nil
	}
	if !validRecoveryPathID(req.PointID) || !validRecoveryPathID(req.RequestUID) || !recoveryDNSName(req.Namespace) || req.ServiceAccount != recoveryapi.WorkloadServiceAccount {
		return fail(errors.New("invalid imported recovery request identity"))
	}
	var point recovery.Manifest
	if err := recoveryJSON(ctx, b.client, "GET", "/points/"+req.PointID, nil, &point); err != nil {
		var httpErr *recoveryHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			return fail(errors.New("imported recovery point is unavailable"))
		}
		return wait(err)
	}
	if point.Scope != "workload" || point.ID != req.PointID || len(point.Members) == 0 || len(point.Members) != len(req.PVCNames) {
		return fail(errors.New("imported workload membership does not match the requested mapping"))
	}
	run := recoveryKubectl(b.client)
	var namespace struct {
		Metadata struct {
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	get := func(resource, name string, out any, impersonate bool) error {
		args := []string{"get", resource, name, "-o", "json"}
		if impersonate {
			args = append(args, "-n", req.Namespace, "--as=system:serviceaccount:"+req.Namespace+":"+req.ServiceAccount)
		}
		raw, err := run(ctx, nil, args...)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, out)
	}
	if err := get("namespace", req.Namespace, &namespace, false); err != nil {
		return wait(err)
	}
	if namespace.Metadata.UID != req.NamespaceUID {
		return fail(errors.New("namespace identity changed during imported restore"))
	}
	granted := false
	for _, id := range strings.Split(namespace.Metadata.Annotations[recoveryapi.Group+"/import-points"], ",") {
		if strings.TrimSpace(id) == point.ID {
			granted = true
		}
	}
	var nodes struct {
		Items []struct {
			Metadata recoveryapi.Metadata `json:"metadata"`
		} `json:"items"`
	}
	raw, err := run(ctx, nil, "get", "nodes", "-o", "json")
	if err != nil {
		return wait(err)
	}
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return fail(err)
	}
	if len(nodes.Items) != 1 {
		return fail(errors.New("imported local-volume restore requires the single appliance node"))
	}
	node := nodes.Items[0].Metadata.Name
	if !recoveryDNSName(node) {
		return fail(errors.New("invalid appliance node identity"))
	}
	seen := map[string]bool{}
	fenced := map[string]bool{}
	var targets []importedTarget
	for _, source := range point.Members {
		name := req.PVCNames[source.PVCName]
		if source.PVCNamespace != req.Namespace || !recoveryDNSName(name) || seen[name] || name == source.PVCName {
			return fail(errors.New("restore requires an exact mapping to distinct new same-namespace PVC names"))
		}
		seen[name] = true
		var meta workloadExportMetadata
		if err := json.Unmarshal(source.Metadata, &meta); err != nil {
			return fail(errors.New("imported point lacks workload recovery metadata"))
		}
		m := meta.Member
		if m.PVC != source.PVCName || m.UID != source.PVCUID || m.SnapshotHandle != source.SnapshotHandle || meta.NamespaceUID == "" {
			return fail(errors.New("imported member metadata identity mismatch"))
		}
		if meta.NamespaceUID != req.NamespaceUID && !granted {
			return fail(errors.New("a cluster administrator must authorize this point on the replacement namespace with the recovery import-points annotation"))
		}
		if m.VolumeMode != "" && m.VolumeMode != "Filesystem" {
			return fail(errors.New("only filesystem PVC imports are supported"))
		}
		if !recoveryDNSName(m.StorageClass) {
			return fail(errors.New("imported storage class name is invalid"))
		}
		var class struct {
			Provisioner string `json:"provisioner"`
		}
		if err := get("storageclass", m.StorageClass, &class, false); err != nil {
			return wait(err)
		}
		if class.Provisioner != "btrfs.csi.local" {
			return fail(errors.New("restore storage class must use the appliance Btrfs CSI driver"))
		}
		var pvc struct {
			Metadata recoveryapi.Metadata `json:"metadata"`
			Spec     struct {
				VolumeName       string `json:"volumeName"`
				StorageClassName string `json:"storageClassName"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		err := get("pvc", name, &pvc, true)
		if recoveryNotFound(err) {
			object := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{
				"name": name, "namespace": req.Namespace,
				"labels":      map[string]string{recoveryOwnerLabel: req.RequestUID, recoveryFenceLabel: "true"},
				"annotations": map[string]string{"volume.kubernetes.io/selected-node": node},
			}, "spec": map[string]any{"storageClassName": m.StorageClass, "accessModes": m.AccessModes, "volumeMode": "Filesystem", "resources": map[string]any{"requests": map[string]string{"storage": m.Storage}}}}
			body, err := json.Marshal(object)
			if err != nil {
				return fail(err)
			}
			if _, err := run(ctx, body, "create", "-f", "-", "-n", req.Namespace, "--as=system:serviceaccount:"+req.Namespace+":"+req.ServiceAccount); err != nil {
				// The create may have reached the API before a response was lost.
				// Retry reads and ownership checks, never replace an existing PVC.
				return recoveryapi.ImportedRestoreStatus{Phase: "Pending", Reason: "AwaitingTarget", Message: "Waiting to verify target PVC creation"}, err
			}
			err = get("pvc", name, &pvc, true)
		}
		if err != nil {
			return wait(err)
		}
		if pvc.Metadata.Labels[recoveryOwnerLabel] != req.RequestUID || pvc.Spec.StorageClassName != m.StorageClass {
			return fail(fmt.Errorf("target PVC %s already exists outside this restore request", name))
		}
		fenced[name] = pvc.Metadata.Labels[recoveryFenceLabel] == "true"
		if pvc.Status.Phase != "Bound" || pvc.Spec.VolumeName == "" {
			return recoveryapi.ImportedRestoreStatus{Phase: "Pending", Reason: "AwaitingProvisioning", Message: "Waiting for new restore PVCs to bind"}, nil
		}
		var pv struct {
			Metadata recoveryapi.Metadata `json:"metadata"`
			Spec     struct {
				ClaimRef struct {
					UID       string `json:"uid"`
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"claimRef"`
				CSI struct {
					Driver       string `json:"driver"`
					VolumeHandle string `json:"volumeHandle"`
				} `json:"csi"`
			} `json:"spec"`
		}
		if err := get("pv", pvc.Spec.VolumeName, &pv, false); err != nil {
			return wait(err)
		}
		if pv.Spec.CSI.Driver != "btrfs.csi.local" || pv.Spec.ClaimRef.UID != pvc.Metadata.UID || pv.Spec.ClaimRef.Namespace != req.Namespace || pv.Spec.ClaimRef.Name != name {
			return fail(errors.New("new target PVC/PV binding differs from the requested identity"))
		}
		targets = append(targets, importedTarget{source.SnapshotHandle, pv.Spec.CSI.VolumeHandle, pvc.Metadata.UID, name, pvc.Spec.VolumeName})
	}
	populate := recovery.PopulateRequest{UID: req.RequestUID, PointID: req.PointID, Namespace: req.Namespace, NamespaceUID: req.NamespaceUID, Targets: targets}
	var prior recovery.Population
	priorErr := recoveryJSON(ctx, b.client, "GET", "/workload-restore/"+req.RequestUID, nil, &prior)
	if priorErr == nil && !reflect.DeepEqual(prior.Request, populate) {
		return fail(errors.New("guest workload transaction belongs to different target identities"))
	}
	if priorErr == nil && prior.Phase == "completed" {
		if err := b.releaseImportedFence(ctx, req.Namespace, targets, fenced); err != nil {
			return wait(err)
		}
		return recoveryapi.ImportedRestoreStatus{Phase: "Succeeded", Reason: "ImportedPVCsPopulated", RestoredPVCs: req.PVCNames}, nil
	}
	if priorErr != nil {
		var missing *recoveryHTTPError
		if !errors.As(priorErr, &missing) || missing.Status != http.StatusNotFound {
			return wait(priorErr)
		}
	}
	for _, target := range targets {
		if !fenced[target.TargetPVCName] {
			return fail(errors.New("restore target lost its consumer fence before population completed"))
		}
		// Confirm the API server's admission cache observes this fence before
		// touching the volume. This creates no pod and pulls no image.
		probe, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]string{"generateName": "recovery-fence-probe-", "namespace": req.Namespace}, "spec": map[string]any{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"containers": []any{map[string]any{"name": "probe", "image": "registry.k8s.io/pause:3.10", "volumeMounts": []any{map[string]string{"name": "data", "mountPath": "/data"}}}},
			"volumes":    []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]string{"claimName": target.TargetPVCName}}},
		}})
		_, err := run(ctx, probe, "create", "--dry-run=server", "-f", "-", "-n", req.Namespace)
		if err == nil || !strings.Contains(err.Error(), "ca-recovery-pvc-consumers") {
			return wait(errors.New("waiting for API admission to enforce the restore consumer fence"))
		}
	}
	// This read is privileged metadata-only inspection: ordinary workloads never
	// gain permission to exec or create pods through imported restore.
	var pods struct {
		Items []struct {
			Spec struct {
				Volumes []struct {
					PVC *struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"items"`
	}
	raw, err = run(ctx, nil, "get", "pods", "-n", req.Namespace, "-o", "json")
	if err != nil {
		return wait(err)
	}
	if err := json.Unmarshal(raw, &pods); err != nil {
		return fail(err)
	}
	for _, pod := range pods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.PVC != nil && seen[volume.PVC.ClaimName] {
				return fail(errors.New("restore target is referenced by a pod; populate it before starting consumers"))
			}
		}
	}
	if err := get("namespace", req.Namespace, &namespace, false); err != nil {
		return wait(err)
	}
	if namespace.Metadata.UID != req.NamespaceUID {
		return fail(errors.New("namespace identity changed before target population"))
	}
	var result recovery.Population
	err = recoveryJSON(ctx, b.client, "POST", "/workload-restore", populate, &result)
	if err != nil {
		var httpErr *recoveryHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
			return fail(errors.New("guest could not safely populate imported volumes; inspect launcher logs"))
		}
		return wait(err)
	}
	if result.Phase != "completed" || !reflect.DeepEqual(result.Request, populate) {
		return fail(errors.New("imported workload population did not complete"))
	}
	if err := b.releaseImportedFence(ctx, req.Namespace, targets, fenced); err != nil {
		return wait(err)
	}
	return recoveryapi.ImportedRestoreStatus{Phase: "Succeeded", Reason: "ImportedPVCsPopulated", Message: "Imported data restored into new PVCs; validate before starting workloads", RestoredPVCs: req.PVCNames}, nil
}

func (b *recoveryBridge) releaseImportedFence(ctx context.Context, namespace string, targets []importedTarget, fenced map[string]bool) error {
	run := recoveryKubectl(b.client)
	for _, target := range targets {
		if !fenced[target.TargetPVCName] {
			continue
		}
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": target.TargetPVCUID},
			{"op": "test", "path": "/metadata/labels/recovery.containersanywhere.dev~1restoring", "value": "true"},
			{"op": "remove", "path": "/metadata/labels/recovery.containersanywhere.dev~1restoring"},
		})
		if _, err := run(ctx, nil, "patch", "pvc", target.TargetPVCName, "-n", namespace, "--type=json", "-p", string(patch)); err != nil {
			return err
		}
	}
	return nil
}
