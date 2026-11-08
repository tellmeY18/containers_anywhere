package recoveryapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

var dnsPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func dnsName(s string) bool {
	return len(s) > 0 && len(s) <= 253 && dnsPattern.MatchString(s) && !strings.Contains(s, "..")
}

func (s CaptureSpec) Validate() error {
	if len(s.Selector.MatchLabels) == 0 || len(s.Selector.MatchLabels) > 16 {
		return errors.New("selector requires 1..16 exact matchLabels")
	}
	if len(s.ExpectedPVCs) == 0 || len(s.ExpectedPVCs) > MaxMembers {
		return errors.New("expectedPVCs requires 1..64 exact member names")
	}
	seen := map[string]bool{}
	for _, n := range s.ExpectedPVCs {
		if !dnsName(n) || seen[n] {
			return errors.New("invalid or duplicate expectedPVC")
		}
		seen[n] = true
	}
	if s.Consistency != "PerPVC" && s.Consistency != "QuiescedGroup" {
		return errors.New("consistency must be PerPVC or QuiescedGroup")
	}
	if s.TimeoutSeconds < 10 || s.TimeoutSeconds > 300 {
		return errors.New("capture timeoutSeconds must be 10..300")
	}
	if len(s.Hooks) > 16 || (s.Consistency == "PerPVC" && len(s.Hooks) != 0) || (s.Consistency == "QuiescedGroup" && len(s.Hooks) == 0) {
		return errors.New("QuiescedGroup requires 1..16 hooks; PerPVC forbids hooks")
	}
	for _, h := range s.Hooks {
		if !dnsName(h.Pod) || !dnsName(h.Container) || h.TimeoutSeconds < 1 || h.TimeoutSeconds > 30 {
			return errors.New("invalid hook target or timeout")
		}
		for _, cmd := range [][]string{h.Prepare, h.Resume, h.Check} {
			if len(cmd) == 0 || len(cmd) > 32 {
				return errors.New("prepare, resume and check commands are required (1..32 arguments)")
			}
			for _, arg := range cmd {
				if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
					return errors.New("invalid hook argument")
				}
			}
		}
	}
	if len(s.ConfigResources) > 32 {
		return errors.New("at most 32 config resources")
	}
	for _, ref := range s.ConfigResources {
		if ref.Kind != "ConfigMap" || !dnsName(ref.Name) {
			return errors.New("only explicit ConfigMap capture is supported")
		}
	}
	return nil
}

type pvc struct {
	Metadata Metadata `json:"metadata"`
	Spec     struct {
		VolumeName       string   `json:"volumeName"`
		StorageClassName string   `json:"storageClassName"`
		AccessModes      []string `json:"accessModes"`
		VolumeMode       string   `json:"volumeMode"`
		Resources        struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// SelectPVCs checks exact membership, not merely a minimum count. The caller
// persists names AND UIDs before hooks and rechecks them before every snapshot.
func SelectPVCs(spec CaptureSpec, raw []byte) ([]Member, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	var list struct {
		Items []pvc `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	if list.Items == nil {
		return nil, errors.New("PVC response has no items")
	}
	expected := map[string]bool{}
	for _, n := range spec.ExpectedPVCs {
		expected[n] = true
	}
	var members []Member
	for _, p := range list.Items {
		match := true
		for k, v := range spec.Selector.MatchLabels {
			if actual, ok := p.Metadata.Labels[k]; !ok || actual != v {
				match = false
			}
		}
		if !match {
			continue
		}
		if !expected[p.Metadata.Name] {
			return nil, fmt.Errorf("selector unexpectedly includes PVC %s", p.Metadata.Name)
		}
		delete(expected, p.Metadata.Name)
		if p.Metadata.UID == "" || p.Metadata.DeletionTimestamp != nil || p.Status.Phase != "Bound" {
			return nil, fmt.Errorf("PVC %s must have a stable UID and be Bound", p.Metadata.Name)
		}
		if p.Spec.StorageClassName != "btrfs" && p.Spec.StorageClassName != "btrfs-nodatacow" && p.Spec.StorageClassName != "local-path" {
			return nil, fmt.Errorf("PVC %s is not a supported local btrfs class", p.Metadata.Name)
		}
		if p.Spec.Resources.Requests["storage"] == "" {
			return nil, errors.New("PVC has no requested capacity")
		}
		members = append(members, Member{PVC: p.Metadata.Name, UID: p.Metadata.UID, VolumeName: p.Spec.VolumeName, StorageClass: p.Spec.StorageClassName, Storage: p.Spec.Resources.Requests["storage"], AccessModes: p.Spec.AccessModes, VolumeMode: p.Spec.VolumeMode})
	}
	if len(expected) != 0 {
		return nil, errors.New("selector does not match every expected PVC")
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PVC < members[j].PVC })
	return members, nil
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func generatedName(prefix, uid, member string) string {
	return prefix + digest([]string{uid, member})[:32]
}

func (c *Controller) workload(ctx context.Context, namespace string, input []byte, args ...string) ([]byte, error) {
	if !dnsName(namespace) {
		return nil, errors.New("workload operation requires a namespace")
	}
	// This fixed identity has no privileges until an administrator explicitly
	// binds a Role in this namespace. Never fall back to the guest's admin token.
	if len(args) == 0 {
		return nil, errors.New("workload operation requires a verb")
	}
	args = append([]string{args[0], "--as=system:serviceaccount:" + namespace + ":" + WorkloadServiceAccount, "--namespace=" + namespace}, args[1:]...)
	return c.run(ctx, input, args...)
}

func (c *Controller) namespaceUID(ctx context.Context, name string) (string, error) {
	b, err := c.run(ctx, nil, "get", "namespace", name, "-o", "json")
	if err != nil {
		return "", err
	}
	var obj struct {
		Metadata Metadata `json:"metadata"`
	}
	if err = json.Unmarshal(b, &obj); err != nil {
		return "", err
	}
	if obj.Metadata.UID == "" || obj.Metadata.DeletionTimestamp != nil {
		return "", errors.New("namespace is missing or deleting")
	}
	return obj.Metadata.UID, nil
}

func (c *Controller) checkNamespace(ctx context.Context, r Record) error {
	uid, err := c.namespaceUID(ctx, r.Namespace)
	if err != nil {
		return err
	}
	if uid != r.NamespaceUID {
		return errors.New("namespace identity changed; retained catalog requires administrator recovery")
	}
	return nil
}

func (c *Controller) selectMembers(ctx context.Context, ns string, s CaptureSpec) ([]Member, error) {
	b, err := c.workload(ctx, ns, nil, "get", "persistentvolumeclaims", "-o", "json")
	if err != nil {
		return nil, err
	}
	return SelectPVCs(s, b)
}

func sameMembers(a, b []Member) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PVC != b[i].PVC || a[i].UID != b[i].UID || a[i].VolumeName != b[i].VolumeName || a[i].StorageClass != b[i].StorageClass || a[i].Storage != b[i].Storage {
			return false
		}
	}
	return true
}

func (c *Controller) podUID(ctx context.Context, ns string, h Hook) (string, error) {
	b, err := c.workload(ctx, ns, nil, "get", "pod", h.Pod, "-o", "json")
	if err != nil {
		return "", err
	}
	var p struct {
		Metadata Metadata `json:"metadata"`
		Spec     struct {
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err = json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	for _, container := range p.Spec.Containers {
		if container.Name == h.Container && p.Metadata.UID != "" && p.Metadata.DeletionTimestamp == nil {
			return p.Metadata.UID, nil
		}
	}
	return "", errors.New("hook container missing or pod deleting")
}

func (c *Controller) execHook(ctx context.Context, r Record, a HookAttempt, cmd []string) error {
	if err := c.checkNamespace(ctx, r); err != nil {
		return err
	}
	uid, err := c.podUID(ctx, r.Namespace, a.Hook)
	if err != nil {
		return err
	}
	if uid != a.PodUID {
		return errors.New("hook pod UID changed; refusing command against replacement")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.Hook.TimeoutSeconds)*time.Second)
	defer cancel()
	args := append([]string{"exec", a.Hook.Pod, "-c", a.Hook.Container, "--"}, cmd...)
	_, err = c.workload(ctx, r.Namespace, nil, args...)
	return err
}

// resume persists each checked success independently. An uncertain prepare is
// always resumed too. A failed check is actionable ResumeFailed, never success.
func (c *Controller) resume(ctx context.Context, r *Record) error {
	var errs []error
	for i := len(r.Hooks) - 1; i >= 0; i-- {
		a := r.Hooks[i]
		if a.Resumed {
			continue
		}
		if err := c.execHook(ctx, *r, a, a.Hook.Resume); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := c.execHook(ctx, *r, a, a.Hook.Check); err != nil {
			errs = append(errs, err)
			continue
		}
		r.Hooks[i].Resumed = true
		if err := c.store().Put(*r); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) capture(ctx context.Context, r *Record, s CaptureSpec) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.TimeoutSeconds)*time.Second)
	defer cancel()
	for _, h := range s.Hooks {
		uid, err := c.podUID(ctx, r.Namespace, h)
		if err != nil {
			return err
		}
		a := HookAttempt{Hook: h, PodUID: uid}
		r.Hooks = append(r.Hooks, a)
		if err := c.store().Put(*r); err != nil {
			return err
		}
		if err := c.execHook(ctx, *r, a, h.Prepare); err != nil {
			return err
		}
	}
	for i := range r.Members {
		if err := c.checkNamespace(ctx, *r); err != nil {
			return err
		}
		members, err := c.selectMembers(ctx, r.Namespace, s)
		if err != nil {
			return err
		}
		if !sameMembers(r.Members, members) {
			return errors.New("PVC membership changed during capture")
		}
		m := &r.Members[i]
		obj := map[string]any{"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot", "metadata": map[string]any{"name": m.Snapshot, "namespace": r.Namespace, "labels": map[string]string{Group + "/request-uid": r.UID}}, "spec": map[string]any{"volumeSnapshotClassName": SnapshotClass, "source": map[string]string{"persistentVolumeClaimName": m.PVC}}}
		if err := c.createExact(ctx, r.Namespace, "volumesnapshots.snapshot.storage.k8s.io", m.Snapshot, obj, r.UID); err != nil {
			return err
		}
		for {
			b, err := c.workload(ctx, r.Namespace, nil, "get", "volumesnapshot", m.Snapshot, "-o", "json")
			if err != nil {
				return err
			}
			var snap struct {
				Metadata Metadata `json:"metadata"`
				Status   struct {
					Ready   bool   `json:"readyToUse"`
					Content string `json:"boundVolumeSnapshotContentName"`
					Error   *struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"status"`
			}
			if err := json.Unmarshal(b, &snap); err != nil {
				return err
			}
			if snap.Metadata.Labels[Group+"/request-uid"] != r.UID || snap.Metadata.UID == "" {
				return errors.New("snapshot identity mismatch")
			}
			if m.SnapshotUID != "" && m.SnapshotUID != snap.Metadata.UID {
				return errors.New("snapshot was replaced")
			}
			m.SnapshotUID, m.Content = snap.Metadata.UID, snap.Status.Content
			if snap.Status.Error != nil {
				return fmt.Errorf("snapshot failed: %s", snap.Status.Error.Message)
			}
			if snap.Status.Ready && snap.Status.Content != "" {
				if err := c.resolveSnapshot(ctx, *r, m); err != nil {
					return err
				}
				m.Ready = true
				break
			}
			if err := c.store().Put(*r); err != nil {
				return err
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := c.store().Put(*r); err != nil {
			return err
		}
	}
	for _, ref := range s.ConfigResources {
		b, err := c.workload(ctx, r.Namespace, nil, "get", "configmap", ref.Name, "-o", "json")
		if err != nil {
			return err
		}
		if len(b) > 1<<20 {
			return errors.New("config resource exceeds 1 MiB")
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		// Store declarative data only, not ownership, resourceVersion or status.
		meta, _ := json.Marshal(map[string]string{"name": ref.Name, "namespace": r.Namespace})
		obj["metadata"] = meta
		delete(obj, "status")
		b, err = json.Marshal(obj)
		if err != nil {
			return err
		}
		if r.Config == nil {
			r.Config = map[string]json.RawMessage{}
		}
		r.Config[ref.Kind+"/"+ref.Name] = b
	}
	return c.store().Put(*r)
}

// createExact uses create, never apply: an existing unrelated PVC/snapshot must
// not be mutated. On retry it verifies both the durable UID label and spec.
func (c *Controller) createExact(ctx context.Context, ns, resource, name string, obj map[string]any, uid string) error {
	b, err := c.workload(ctx, ns, nil, "get", resource, name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		input, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		_, err = c.workload(ctx, ns, input, "create", "-f", "-")
		return err
	}
	var existing struct {
		Metadata Metadata       `json:"metadata"`
		Spec     map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(b, &existing); err != nil {
		return err
	}
	if existing.Metadata.Labels[Group+"/request-uid"] != uid || existing.Metadata.DeletionTimestamp != nil {
		return errors.New("output name is already in use")
	}
	// Kubernetes adds defaults/binding fields. Compare every field we supplied.
	wantBytes, _ := json.Marshal(obj["spec"])
	var want map[string]any
	_ = json.Unmarshal(wantBytes, &want)
	for k, v := range want {
		if !reflect.DeepEqual(existing.Spec[k], v) {
			return fmt.Errorf("existing output differs at spec.%s", k)
		}
	}
	return nil
}
