package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var uidName = regexp.MustCompile(`^[0-9a-f-]{8,64}$`)

func (c *Controller) setStatus(ctx context.Context, s Object, phase, message string) error {
	st := Object{"phase": phase, "message": message}
	if phase == "Ready" {
		st["createdAt"] = c.Now().UTC().Format(time.RFC3339)
	}
	return c.Kube.SetStatus(ctx, snapshotRes, str(s, "metadata", "namespace"), str(s, "metadata", "name"), st)
}

func byCreated(list []Object) {
	sort.SliceStable(list, func(i, j int) bool {
		return str(list[i], "metadata", "creationTimestamp") < str(list[j], "metadata", "creationTimestamp")
	})
}

// snapshots takes the read-only snapshot each new PVCSnapshot asks for. A failure
// is final for that object (the reason is in its status); a schedule simply makes
// the next one.
func (c *Controller) snapshots(ctx context.Context, snaps, pvcs []Object, pvs map[string]Object) error {
	claims := map[string]Object{}
	for _, p := range pvcs {
		claims[key(str(p, "metadata", "namespace"), str(p, "metadata", "name"))] = p
	}
	used := map[string]int{}
	for _, s := range snaps {
		if str(s, "status", "phase") == "Ready" {
			used[str(s, "metadata", "namespace")]++
		}
	}
	ordered := append([]Object(nil), snaps...)
	byCreated(ordered)
	var errs []error
	for _, s := range ordered {
		phase := str(s, "status", "phase")
		if phase == "Ready" || phase == "Failed" || str(s, "metadata", "deletionTimestamp") != "" {
			continue
		}
		ns, uid := str(s, "metadata", "namespace"), str(s, "metadata", "uid")
		fail := func(msg string) {
			if err := c.setStatus(ctx, s, "Failed", msg); err != nil {
				errs = append(errs, err)
			}
		}
		if !validName(ns) || !uidName.MatchString(uid) {
			fail("invalid object")
			continue
		}
		if used[ns] >= c.MaxPerNamespace {
			fail(fmt.Sprintf("this namespace already holds the maximum of %d snapshots", c.MaxPerNamespace))
			continue
		}
		claim, ok := claims[key(ns, str(s, "spec", "persistentVolumeClaimName"))]
		pv := pvs[str(claim, "spec", "volumeName")]
		src := str(pv, "metadata", "annotations", annPath)
		if !ok || pv == nil || obj(pv, "metadata", "labels")[labelManaged] != "true" || !within(c.VolumesDir, src) {
			fail("the claim does not exist or is not a Containers Anywhere btrfs volume")
			continue
		}
		if low, err := c.belowReserve(); err != nil {
			errs = append(errs, err)
			continue
		} else if low {
			fail("not enough free space on the data disk to keep another snapshot")
			continue
		}
		dst := c.snapshotPath(uid)
		if !c.FS.IsSubvolume(ctx, dst) {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := c.FS.Snapshot(ctx, src, dst, true); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := c.setStatus(ctx, s, "Ready", ""); err != nil {
			errs = append(errs, err)
			continue
		}
		used[ns]++
	}
	return errors.Join(errs...)
}

// schedules creates a PVCSnapshot each interval and removes the oldest beyond keep.
func (c *Controller) schedules(ctx context.Context, scheds, snaps []Object) error {
	var errs []error
	for _, sc := range scheds {
		ns, name, uid := str(sc, "metadata", "namespace"), str(sc, "metadata", "name"), str(sc, "metadata", "uid")
		claim := str(sc, "spec", "persistentVolumeClaimName")
		every, keep := num(sc, "spec", "intervalMinutes"), num(sc, "spec", "keep")
		if !validName(ns) || !validName(name) || !validName(claim) || every < 5 || every > 10080 || keep < 1 || keep > 10 ||
			str(sc, "metadata", "deletionTimestamp") != "" {
			continue
		}
		var children []Object
		for _, s := range snaps {
			if str(s, "metadata", "namespace") == ns && obj(s, "metadata", "labels")[labelSchedule] == name {
				children = append(children, s)
			}
		}
		byCreated(children)
		now := c.Now()
		due := len(children) == 0
		if !due {
			last, err := time.Parse(time.RFC3339, str(children[len(children)-1], "metadata", "creationTimestamp"))
			due = err != nil || now.Sub(last) >= time.Duration(every)*time.Minute
		}
		if due {
			child := fmt.Sprintf("%s-%s", name, now.UTC().Format("20060102-1504"))
			if len(child) <= 253 {
				err := c.Kube.Apply(ctx, Object{
					"apiVersion": Group + "/" + Version, "kind": SnapshotKind,
					"metadata": Object{"name": child, "namespace": ns, "labels": Object{labelSchedule: name},
						"ownerReferences": []any{Object{"apiVersion": Group + "/" + Version, "kind": ScheduleKind, "name": name, "uid": uid, "controller": true}}},
					"spec": Object{"persistentVolumeClaimName": claim},
				})
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
		for i := 0; i < len(children)-keep; i++ {
			if p := str(children[i], "status", "phase"); p != "Ready" && p != "Failed" {
				continue
			}
			if err := c.Kube.Delete(ctx, snapshotRes, ns, str(children[i], "metadata", "name")); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// collect deletes snapshot subvolumes whose PVCSnapshot no longer exists. It only
// runs after the objects were listed successfully.
func (c *Controller) collect(ctx context.Context, snaps []Object) error {
	live := map[string]bool{}
	for _, s := range snaps {
		live[str(s, "metadata", "uid")] = true
	}
	dir := filepath.Join(c.SnapshotsDir, "pvc")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if live[e.Name()] || !uidName.MatchString(e.Name()) || !within(c.SnapshotsDir, p) {
			continue
		}
		if c.FS.IsSubvolume(ctx, p) {
			if err := c.FS.DeleteSubvolume(ctx, p); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
