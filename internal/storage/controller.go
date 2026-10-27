package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	Group         = "storage.containersanywhere.dev"
	Version       = "v1alpha1"
	Provisioner   = "containers-anywhere.dev/btrfs"
	SnapshotKind  = "PVCSnapshot"
	ScheduleKind  = "PVCSnapshotSchedule"
	snapshotRes   = "pvcsnapshots." + Group
	scheduleRes   = "pvcsnapshotschedules." + Group
	labelManaged  = "containers-anywhere.dev/managed-volume"
	labelSchedule = Group + "/schedule"
	annPath       = "containers-anywhere.dev/path"
)

var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// Controller turns PersistentVolumeClaims of the Containers Anywhere classes into
// btrfs subvolumes and serves the snapshot API. Each pass is idempotent, so a crash
// at any point is repaired by the next one.
type Controller struct {
	Kube         Kube
	FS           FS
	VolumesDir   string // the @volumes subvolume
	SnapshotsDir string // the @snapshots subvolume
	// Reserve is the free fraction below which new snapshots and clones are refused,
	// so one workload cannot consume the space the appliance itself needs.
	Reserve float64
	// MaxPerNamespace bounds the snapshots one namespace may hold.
	MaxPerNamespace int
	Now             func() time.Time
	Log             func(format string, args ...any)

	mu      sync.Mutex
	lastErr string
}

func NewController(kube Kube, fs FS, volumes, snapshots string) *Controller {
	return &Controller{Kube: kube, FS: fs, VolumesDir: volumes, SnapshotsDir: snapshots, Reserve: 0.15, MaxPerNamespace: 16,
		Now: time.Now, Log: func(string, ...any) {}}
}

// Run reconciles until the context ends. It logs an error once while it persists.
func (c *Controller) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		err := c.Reconcile(ctx)
		c.mu.Lock()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != c.lastErr {
			if msg != "" {
				c.Log("storage: %s", msg)
			} else {
				c.Log("storage: recovered")
			}
			c.lastErr = msg
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func str(o Object, path ...string) string {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(Object)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

func obj(o Object, path ...string) Object {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(Object)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	m, _ := cur.(Object)
	return m
}

func num(o Object, path ...string) int {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(Object)
		if !ok {
			return 0
		}
		cur = m[p]
	}
	switch n := cur.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func short(uid string) string {
	uid = strings.ReplaceAll(uid, "-", "")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	return uid
}

func validName(s string) bool { return dnsName.MatchString(s) && !strings.Contains(s, "..") }

func (c *Controller) volumePath(ns, name, uid string) string {
	return filepath.Join(c.VolumesDir, ns, name+"-"+short(uid))
}

func (c *Controller) snapshotPath(uid string) string {
	return filepath.Join(c.SnapshotsDir, "pvc", uid)
}

func key(ns, name string) string { return ns + "/" + name }

// Reconcile runs one pass. A failure in one stage never stops the others.
func (c *Controller) Reconcile(ctx context.Context) error {
	classes, err := c.Kube.List(ctx, "storageclass")
	if err != nil {
		return err
	}
	ours := map[string]bool{} // class -> copy-on-write
	for _, sc := range classes {
		if str(sc, "provisioner") == Provisioner {
			ours[str(sc, "metadata", "name")] = str(sc, "parameters", "cow") != "false"
		}
	}
	pvcs, err := c.Kube.List(ctx, "pvc")
	if err != nil {
		return err
	}
	pvs, err := c.Kube.List(ctx, "pv")
	if err != nil {
		return err
	}
	snaps, snapErr := c.Kube.List(ctx, snapshotRes)
	scheds, schedErr := c.Kube.List(ctx, scheduleRes)

	var errs []error
	pvByName := map[string]Object{}
	pvByClaim := map[string]bool{}
	for _, pv := range pvs {
		pvByName[str(pv, "metadata", "name")] = pv
		if obj(pv, "metadata", "labels")[labelManaged] == "true" {
			pvByClaim[str(pv, "spec", "claimRef", "uid")] = true
		}
	}
	snapByName := map[string]Object{}
	for _, s := range snaps {
		snapByName[key(str(s, "metadata", "namespace"), str(s, "metadata", "name"))] = s
	}
	for _, pvc := range pvcs {
		if err := c.provision(ctx, pvc, ours, pvByClaim, snapByName); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.cleanup(ctx, pvs, pvcs); err != nil {
		errs = append(errs, err)
	}
	if snapErr != nil || schedErr != nil {
		// The snapshot API is not installed yet. Do nothing, and above all do not
		// collect snapshots whose objects we simply failed to read.
		return errors.Join(errs...)
	}
	if err := c.snapshots(ctx, snaps, pvcs, pvByName); err != nil {
		errs = append(errs, err)
	}
	if err := c.schedules(ctx, scheds, snaps); err != nil {
		errs = append(errs, err)
	}
	if err := c.collect(ctx, snaps); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *Controller) belowReserve() (bool, error) {
	free, err := c.FS.FreeFraction(c.VolumesDir)
	if err != nil {
		return false, err
	}
	return free < c.Reserve, nil
}

func (c *Controller) provision(ctx context.Context, pvc Object, ours map[string]bool, pvByClaim map[string]bool, snapByName map[string]Object) error {
	class := str(pvc, "spec", "storageClassName")
	cow, mine := ours[class]
	uid, ns, name := str(pvc, "metadata", "uid"), str(pvc, "metadata", "namespace"), str(pvc, "metadata", "name")
	if !mine || str(pvc, "metadata", "deletionTimestamp") != "" || str(pvc, "spec", "volumeName") != "" ||
		(str(pvc, "status", "phase") != "Pending" && str(pvc, "status", "phase") != "") ||
		str(pvc, "spec", "volumeMode") == "Block" || pvByClaim[uid] {
		return nil
	}
	if !validName(ns) || !validName(name) || uid == "" {
		return fmt.Errorf("refusing claim with an unusual name: %s/%s", ns, name)
	}
	path := c.volumePath(ns, name, uid)
	if !within(c.VolumesDir, path) {
		return fmt.Errorf("volume path escapes the volumes directory: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if !c.FS.IsSubvolume(ctx, path) {
		if src := obj(pvc, "spec", "dataSourceRef"); src != nil {
			if str(src, "apiGroup") != Group || str(src, "kind") != SnapshotKind {
				return fmt.Errorf("%s/%s: only a %s can be the data source of a Containers Anywhere volume", ns, name, SnapshotKind)
			}
			snap, ok := snapByName[key(ns, str(src, "name"))]
			if !ok || str(snap, "status", "phase") != "Ready" {
				return nil // wait for the snapshot, in the same namespace only
			}
			if low, err := c.belowReserve(); err != nil || low {
				return fmt.Errorf("%s/%s: not enough free space to clone a snapshot", ns, name)
			}
			if err := c.FS.Snapshot(ctx, c.snapshotPath(str(snap, "metadata", "uid")), path, false); err != nil {
				return err
			}
		} else {
			if err := c.FS.CreateSubvolume(ctx, path); err != nil {
				return err
			}
			if !cow {
				if err := c.FS.SetNoCOW(ctx, path); err != nil {
					return err
				}
			}
		}
	}
	modes, _ := pvc["spec"].(Object)["accessModes"].([]any)
	if len(modes) == 0 {
		modes = []any{"ReadWriteOnce"}
	}
	size := str(pvc, "spec", "resources", "requests", "storage")
	if size == "" {
		size = "1Gi"
	}
	return c.Kube.Apply(ctx, Object{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": Object{"name": "pvc-" + uid, "labels": Object{labelManaged: "true"}, "annotations": Object{annPath: path}},
		"spec": Object{
			"capacity": Object{"storage": size}, "accessModes": modes,
			"persistentVolumeReclaimPolicy": "Retain", "storageClassName": class, "volumeMode": "Filesystem",
			"hostPath": Object{"path": path, "type": "Directory"},
			"claimRef": Object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": ns, "name": name, "uid": uid},
		},
	})
}

// cleanup removes the subvolume of a managed volume whose claim is gone. A bound
// volume always has a live claim, so only unbound phases are considered.
func (c *Controller) cleanup(ctx context.Context, pvs, pvcs []Object) error {
	live := map[string]bool{}
	for _, pvc := range pvcs {
		live[str(pvc, "metadata", "uid")] = true
	}
	var errs []error
	for _, pv := range pvs {
		if obj(pv, "metadata", "labels")[labelManaged] != "true" {
			continue
		}
		switch str(pv, "status", "phase") {
		case "Released", "Available", "Failed", "Pending", "":
		default:
			continue
		}
		if live[str(pv, "spec", "claimRef", "uid")] {
			continue
		}
		path := str(pv, "metadata", "annotations", annPath)
		if !within(c.VolumesDir, path) {
			errs = append(errs, fmt.Errorf("not removing %q: outside the volumes directory", path))
			continue
		}
		if _, err := os.Lstat(path); err == nil {
			if !c.FS.IsSubvolume(ctx, path) {
				errs = append(errs, fmt.Errorf("not removing %q: not a subvolume", path))
				continue
			}
			if err := c.FS.DeleteSubvolume(ctx, path); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := c.Kube.Delete(ctx, "pv", "", str(pv, "metadata", "name")); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
