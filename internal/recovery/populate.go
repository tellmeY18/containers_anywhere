package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

type PopulateTarget struct {
	SourceSnapshotHandle string `json:"sourceSnapshotHandle"`
	TargetVolumeHandle   string `json:"targetVolumeHandle"`
	TargetPVCUID         string `json:"targetPVCUID"`
	TargetPVCName        string `json:"targetPVCName"`
	TargetPVName         string `json:"targetPVName"`
}
type PopulateRequest struct {
	UID          string           `json:"uid"`
	PointID      string           `json:"pointID"`
	Namespace    string           `json:"namespace"`
	NamespaceUID string           `json:"namespaceUID"`
	Targets      []PopulateTarget `json:"targets"`
}
type PopulateTargetResult struct {
	Target PopulateTarget `json:"target"`
	Phase  string         `json:"phase"`
	Error  string         `json:"error,omitempty"`
}
type Population struct {
	Version   int                    `json:"version"`
	Request   PopulateRequest        `json:"request"`
	Phase     string                 `json:"phase"`
	Targets   []PopulateTargetResult `json:"targets"`
	Error     string                 `json:"error,omitempty"`
	UpdatedAt time.Time              `json:"updatedAt"`
}
type volumeClaim struct {
	UID     string         `json:"uid"`
	PointID string         `json:"pointID"`
	Target  PopulateTarget `json:"target"`
}

func (e *Engine) populationPath(uid string) string {
	return filepath.Join(e.cfg.Root, "populations", key(uid)+".json")
}
func (e *Engine) savePopulation(p *Population) error {
	p.UpdatedAt = e.cfg.Now().UTC()
	return writeRecord(e.populationPath(p.Request.UID), p)
}
func (e *Engine) PopulationStatus(uid string) (Population, error) {
	unlock, err := e.lock()
	if err != nil {
		return Population{}, err
	}
	defer unlock()
	var p Population
	err = readRecord(e.populationPath(uid), &p)
	if err == nil && (p.Version != Version || p.Request.UID != uid) {
		err = errors.New("invalid population journal")
	}
	return p, err
}

// PopulateWorkload copies immutable imported/local workload snapshots only into
// NEW, empty, CSI-created volumes. It never replaces a CSI subvolume or rewrites
// driver state/quotas. The authenticated controller must authorize the namespace,
// verify request-owned fresh PVCs, and fence consuming pods before this call.
// Kernel mount checks supplement, but cannot atomically replace, that fencing.
func (e *Engine) PopulateWorkload(ctx context.Context, req PopulateRequest) (Population, error) {
	if !identifier.MatchString(req.UID) || !identifier.MatchString(req.PointID) || !identifier.MatchString(req.Namespace) || !identifier.MatchString(req.NamespaceUID) || len(req.Targets) == 0 || len(req.Targets) > MaxMembers {
		return Population{}, errors.New("invalid workload population request")
	}
	unlock, err := e.lock()
	if err != nil {
		return Population{}, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return Population{}, err
	}
	ops, err := e.operations()
	if err != nil {
		return Population{}, err
	}
	for _, op := range ops {
		if active(op) {
			return Population{}, ErrBusy
		}
	}
	if err := e.requireNoPopulation(req.UID); err != nil {
		return Population{}, err
	}
	var p Population
	readErr := readRecord(e.populationPath(req.UID), &p)
	if readErr == nil {
		if p.Version != Version || !reflect.DeepEqual(p.Request, req) {
			return p, errors.New("population UID reused for another request")
		}
		if p.Phase == "completed" {
			return p, nil
		}
		if p.Phase == "failed" {
			return p, errors.New(p.Error)
		}
		if p.Phase != "populating" || len(p.Targets) != len(req.Targets) {
			return p, errors.New("invalid population journal")
		}
	} else if !os.IsNotExist(readErr) {
		return p, readErr
	}
	terminal := func(cause error) (Population, error) {
		if readErr == nil && p.Phase == "populating" {
			p.Phase = "failed"
			p.Error = cause.Error()
			if err := e.savePopulation(&p); err != nil {
				return p, errors.Join(cause, err)
			}
		}
		return p, cause
	}
	if err := e.checkCapacity(metadataHeadroom); err != nil {
		return terminal(err)
	}
	m, err := e.get(req.PointID)
	if err != nil {
		return terminal(err)
	}
	if m.Scope != "workload" {
		return terminal(errors.New("population requires a workload point"))
	}
	if err := e.compatible(m); err != nil {
		return terminal(err)
	}
	if err := e.verify(ctx, e.pointPath(m.ID), m); err != nil {
		return terminal(err)
	}
	if len(req.Targets) != len(m.Members) {
		return terminal(errors.New("target set must cover every captured workload member exactly once"))
	}
	members := map[string]SnapshotMember{}
	for _, member := range m.Members {
		members[member.SnapshotHandle] = member
	}
	s, _, err := readDriver(filepath.Join(e.cfg.PoolDir, "state.json"))
	if err != nil {
		return terminal(err)
	}
	seenSources, seenTargets := map[string]bool{}, map[string]bool{}
	for _, target := range req.Targets {
		member, ok := members[target.SourceSnapshotHandle]
		v := s.Volumes[target.TargetVolumeHandle]
		if !ok || seenSources[target.SourceSnapshotHandle] || seenTargets[target.TargetVolumeHandle] || !identifier.MatchString(target.TargetVolumeHandle) || !identifier.MatchString(target.TargetPVCUID) || !identifier.MatchString(target.TargetPVCName) || req.Namespace != member.PVCNamespace {
			return terminal(errors.New("invalid, duplicate or cross-namespace workload target"))
		}
		if target.TargetPVCUID == member.PVCUID || target.TargetVolumeHandle == member.VolumeHandle || v == nil || v.ID != target.TargetVolumeHandle || v.Name != "pvc-"+target.TargetPVCUID || target.TargetPVName != v.Name || v.CapacityBytes <= 0 {
			return terminal(errors.New("target is not a distinct CSI volume bound to the declared new PVC"))
		}
		if err := e.targetIdle(ctx, target); err != nil {
			return terminal(err)
		}
		seenSources[target.SourceSnapshotHandle] = true
		seenTargets[target.TargetVolumeHandle] = true
	}
	if os.IsNotExist(readErr) {
		p = Population{Version: Version, Request: req, Phase: "populating"}
		for _, target := range req.Targets {
			p.Targets = append(p.Targets, PopulateTargetResult{Target: target, Phase: "pending"})
		}
		if err := e.savePopulation(&p); err != nil {
			return p, err
		}
	}
	fail := func(i int, cause error) (Population, error) {
		p.Phase = "failed"
		p.Error = cause.Error()
		p.Targets[i].Phase = "failed"
		p.Targets[i].Error = cause.Error()
		if err := e.savePopulation(&p); err != nil {
			return p, errors.Join(cause, err)
		}
		return p, cause
	}
	for i, target := range req.Targets {
		result := &p.Targets[i]
		if result.Target != target {
			return p, errors.New("population journal target identity mismatch")
		}
		if result.Phase == "completed" {
			continue
		}
		if result.Phase != "pending" && result.Phase != "copying" {
			return p, errors.New("invalid target journal phase")
		}
		if err := e.checkCapacity(metadataHeadroom); err != nil {
			return fail(i, err)
		}
		path := filepath.Join(e.cfg.PoolDir, "volumes", target.TargetVolumeHandle)
		claimPath := filepath.Join(e.cfg.Root, "volume-claims", target.TargetVolumeHandle+".json")
		claim := volumeClaim{req.UID, req.PointID, target}
		var existing volumeClaim
		if err := readRecord(claimPath, &existing); err == nil {
			if existing != claim {
				return fail(i, errors.New("target volume is already claimed by another recovery request"))
			}
		} else if !os.IsNotExist(err) {
			return fail(i, err)
		} else if result.Phase == "copying" {
			return fail(i, errors.New("copying journal lost its durable target claim"))
		}
		if err := e.targetIdle(ctx, target); err != nil {
			return fail(i, err)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return fail(i, err)
		}
		if len(entries) != 0 {
			return fail(i, errors.New("target is nonempty or an interrupted copy is present; contents preserved, create fresh replacement PVCs and a new request UID"))
		}
		if err := writeRecord(claimPath, claim); err != nil {
			return fail(i, err)
		}
		result.Phase = "copying"
		if err := e.savePopulation(&p); err != nil {
			return p, err
		}
		// Recheck after durable ownership publication, immediately before copying.
		if err := e.targetIdle(ctx, target); err != nil {
			return fail(i, err)
		}
		if err := e.sameTargetDriver(target, s.Volumes[target.TargetVolumeHandle]); err != nil {
			return fail(i, err)
		}
		if err := e.cfg.Copy(ctx, filepath.Join(e.pointPath(m.ID), "snap-"+target.SourceSnapshotHandle), path); err != nil {
			return fail(i, fmt.Errorf("target copy failed; partial data retained: %w", err))
		}
		if err := e.cfg.FS.Sync(ctx, path); err != nil {
			return fail(i, err)
		}
		if err := e.checkCapacity(metadataHeadroom); err != nil {
			return fail(i, err)
		}
		if err := e.targetIdle(ctx, target); err != nil {
			return fail(i, err)
		}
		if err := e.sameTargetDriver(target, s.Volumes[target.TargetVolumeHandle]); err != nil {
			return fail(i, err)
		}
		result.Phase = "completed"
		if err := e.savePopulation(&p); err != nil {
			return p, err
		}
	}
	p.Phase = "completed"
	err = e.savePopulation(&p)
	return p, err
}

func (e *Engine) targetIdle(ctx context.Context, target PopulateTarget) error {
	path := filepath.Join(e.cfg.PoolDir, "volumes", target.TargetVolumeHandle)
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("target CSI volume is not a real directory")
	}
	if !e.cfg.FS.IsSubvolume(ctx, path) {
		return errors.New("target CSI volume is missing or not a subvolume")
	}
	ro, err := e.cfg.FS.ReadOnly(ctx, path)
	if err != nil {
		return err
	}
	if ro {
		return errors.New("target CSI volume is read-only")
	}
	mounted, err := e.cfg.FS.Mounted(ctx, path)
	if err != nil {
		return err
	}
	if mounted {
		return errors.New("target CSI volume has an active mount; remove/fence all consuming pods first")
	}
	return nil
}
func (e *Engine) sameTargetDriver(target PopulateTarget, want *driverVolume) error {
	s, _, err := readDriver(filepath.Join(e.cfg.PoolDir, "state.json"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(s.Volumes[target.TargetVolumeHandle], want) {
		return errors.New("target CSI volume identity/capacity changed during population")
	}
	return nil
}

func (e *Engine) populations() ([]Population, error) {
	es, err := os.ReadDir(filepath.Join(e.cfg.Root, "populations"))
	if err != nil {
		return nil, err
	}
	out := []Population{}
	for _, ent := range es {
		if filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		var p Population
		if err := readRecord(filepath.Join(e.cfg.Root, "populations", ent.Name()), &p); err != nil {
			return nil, err
		}
		if p.Version != Version || ent.Name() != key(p.Request.UID)+".json" || (p.Phase != "populating" && p.Phase != "completed" && p.Phase != "failed") {
			return nil, errors.New("invalid population journal")
		}
		out = append(out, p)
	}
	return out, nil
}
func (e *Engine) requireNoPopulation(except string) error {
	ps, err := e.populations()
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Phase == "populating" && p.Request.UID != except {
			return ErrBusy
		}
	}
	return nil
}
