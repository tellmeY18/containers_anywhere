package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/tellmeY18/containers_anywhere/internal/storage"
)

func (e *Engine) Submit(ctx context.Context, req Request) (Operation, error) {
	if req.UID == "" {
		req.UID = req.ID
	}
	if req.ID != "" && req.ID != req.UID {
		return Operation{}, errors.New("id and uid disagree")
	}
	req.ID = req.UID
	if !identifier.MatchString(req.UID) || (req.Action != "capture" && req.Action != "restore") || (req.Scope != "system" && req.Scope != "full") {
		return Operation{}, errors.New("id, action capture|restore, and scope system|full are required")
	}
	if (req.Action == "restore" && !identifier.MatchString(req.PointID)) || (req.Action == "capture" && req.PointID != "") {
		return Operation{}, errors.New("pointID is required only for restore")
	}
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	unlock, err := e.lock()
	if err != nil {
		return Operation{}, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	var old Operation
	if err := readRecord(e.operationPath(req.UID), &old); err == nil {
		if old.Request != req {
			return old, errors.New("idempotency UID already used for a different request")
		}
		return old, nil
	} else if !os.IsNotExist(err) {
		return old, err
	}
	ops, err := e.operations()
	if err != nil {
		return Operation{}, err
	}
	for _, op := range ops {
		if active(op) {
			return Operation{}, ErrBusy
		}
	}
	if err := e.requireNoPopulation(""); err != nil {
		return Operation{}, err
	}
	if req.Action == "restore" {
		m, err := e.get(req.PointID)
		if err != nil {
			return Operation{}, err
		}
		if m.Scope != req.Scope {
			return Operation{}, errors.New("restore scope must equal the point scope")
		}
		if err := e.compatible(m); err != nil {
			return Operation{}, err
		}
	} else if _, err := e.get(key(req.UID)); !os.IsNotExist(err) {
		return Operation{}, errors.New("capture UID already belongs to a published or damaged point")
	}
	op := Operation{Version: Version, Request: req, Phase: "restart-needed", CreatedAt: e.cfg.Now().UTC()}
	if req.Action == "capture" {
		op.PointID = key(req.UID)
	} else {
		op.PointID = req.PointID
	}
	err = e.save(&op)
	return op, err
}

// ProcessCold is the only method that changes live system state. Any unreadable
// journal fails closed: it may describe an interrupted destructive phase.
func (e *Engine) ProcessCold(ctx context.Context) error {
	if e.cfg.Cold == nil {
		return errors.New("cold recovery is only permitted in init-data")
	}
	if err := e.cfg.Cold(ctx); err != nil {
		return err
	}
	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	ops, err := e.operations()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInconsistent, err)
	}
	var pending []Operation
	for _, op := range ops {
		if active(op) {
			pending = append(pending, op)
		}
	}
	if len(pending) > 1 {
		return fmt.Errorf("%w: multiple active operations", ErrInconsistent)
	}
	for i := range pending {
		if err := e.process(ctx, &pending[i]); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) fail(op *Operation, err error) error {
	op.Phase = "failed"
	op.Error = err.Error()
	return e.save(op)
}
func (e *Engine) process(ctx context.Context, op *Operation) error {
	if op.Phase == "restart-needed" || op.Phase == "capturing" {
		var writer bootIdentity
		if err := readRecord(filepath.Join(e.cfg.Root, "boot.json"), &writer); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: unreadable last-writer identity", ErrInconsistent)
		}
		if writer.Appliance != "" && (writer.Appliance != e.cfg.Appliance || writer.K3s != e.cfg.K3s) {
			return e.fail(op, errors.New("pending recovery requires the exact appliance/K3s that last booted this state; do not combine recovery with an appliance update"))
		}
	}
	if op.Request.Action == "capture" {
		if op.Phase != "restart-needed" && op.Phase != "capturing" {
			return fmt.Errorf("%w: invalid capture phase", ErrInconsistent)
		}
		op.Phase = "capturing"
		if err := e.save(op); err != nil {
			return err
		}
		if m, err := e.get(op.PointID); os.IsNotExist(err) {
			if err := e.capture(ctx, op.PointID, op.Request.Scope); err != nil {
				return e.fail(op, err)
			}
		} else if err != nil {
			return e.fail(op, err)
		} else if m.Scope != op.Request.Scope {
			return e.fail(op, errors.New("capture scope mismatch"))
		} else if err := e.verify(ctx, e.pointPath(m.ID), m); err != nil {
			return e.fail(op, err)
		}
		op.Phase = "completed"
		return e.save(op)
	}
	if op.Request.Action != "restore" {
		return fmt.Errorf("%w: invalid action", ErrInconsistent)
	}
	if op.Phase == "restart-needed" {
		m, err := e.get(op.PointID)
		if err != nil {
			return e.fail(op, err)
		}
		if err := e.verify(ctx, e.pointPath(m.ID), m); err != nil {
			return e.fail(op, err)
		}
		if err := e.compatible(m); err != nil {
			return e.fail(op, err)
		}
		if m.Scope != op.Request.Scope {
			return e.fail(op, errors.New("restore scope mismatch"))
		}
		if m.Scope == "system" {
			if err := e.mappingMatches(ctx, m); err != nil {
				return e.fail(op, err)
			}
		}
		op.SafetyID = "safety-" + key(op.Request.UID)
		// Reuse a fully published safety point after a crash between its
		// publication and the restoring journal transition. Never overwrite it.
		if _, err := e.get(op.SafetyID); os.IsNotExist(err) {
			if err := e.capture(ctx, op.SafetyID, "full"); err != nil {
				return e.fail(op, fmt.Errorf("cannot preserve safety point: %w", err))
			}
		} else if err != nil {
			return e.fail(op, err)
		}
		op.Phase = "restoring"
		if err := e.save(op); err != nil {
			return err
		}
	}
	if op.SafetyID == "" || !identifier.MatchString(op.SafetyID) {
		return fmt.Errorf("%w: missing safety identity", ErrInconsistent)
	}
	// Validate safety before touching data, including on restart.
	safety, err := e.get(op.SafetyID)
	if err != nil {
		return fmt.Errorf("%w: safety point: %v", ErrInconsistent, err)
	}
	if safety.Scope != "full" || op.SafetyID != "safety-"+key(op.Request.UID) {
		return fmt.Errorf("%w: invalid safety point identity", ErrInconsistent)
	}
	if err := e.verify(ctx, e.pointPath(safety.ID), safety); err != nil {
		return fmt.Errorf("%w: safety point: %v", ErrInconsistent, err)
	}
	if op.Phase == "restoring" {
		m, err := e.get(op.PointID)
		if err == nil {
			err = e.restore(ctx, m)
		}
		if err == nil {
			op.Phase = "completed"
			return e.save(op)
		}
		op.Error = err.Error()
		op.Phase = "rolling-back"
		if err := e.save(op); err != nil {
			return fmt.Errorf("%w: %v", ErrInconsistent, err)
		}
	}
	if op.Phase != "rolling-back" {
		return fmt.Errorf("%w: invalid restore phase", ErrInconsistent)
	}
	if err := e.restore(ctx, safety); err != nil {
		return fmt.Errorf("%w: rollback: %v", ErrInconsistent, err)
	}
	op.Phase = "failed"
	op.Error += "; previous state restored from " + op.SafetyID
	return e.save(op)
}

func (e *Engine) compatible(m Manifest) error {
	if m.Scope == "workload" {
		if m.DataFormat != e.cfg.DataFormat || m.Architecture != e.cfg.Architecture {
			return errors.New("incompatible workload point format/architecture")
		}
		return nil
	}
	if m.DataFormat != e.cfg.DataFormat || m.Datastore.K3s != e.cfg.K3s || m.Architecture != e.cfg.Architecture || m.Appliance != e.cfg.Appliance {
		return errors.New("point requires its exact data format, K3s, architecture and appliance version")
	}
	if m.Datastore.Kind != "sqlite" || m.Datastore.AdapterVersion != 1 {
		return errors.New("unsupported datastore adapter; etcd capture/restore is not implemented")
	}
	return nil
}

func captures(scope string, in Inventory) []string {
	if scope == "workload" {
		out := []string{}
		for _, id := range in.Snapshots {
			out = append(out, "snap-"+id)
		}
		return out
	}
	out := []string{"system"}
	if scope == "full" && in.PoolPresent {
		out = append(out, "pool")
		for _, id := range in.Volumes {
			out = append(out, "vol-"+id)
		}
		for _, id := range in.Snapshots {
			out = append(out, "snap-"+id)
		}
	}
	sort.Strings(out)
	return out
}
func (e *Engine) validateManifest(m Manifest) error {
	if m.Version != Version || !identifier.MatchString(m.ID) || (m.Scope != "system" && m.Scope != "full" && m.Scope != "workload") || m.DataFormat == "" || m.CreatedAt.IsZero() {
		return errors.New("unsupported or incomplete point manifest")
	}
	if m.Scope != "workload" && (m.Datastore.K3s == "" || m.Datastore.Kind != "sqlite" || m.Datastore.AdapterVersion != 1 || len(m.Datastore.MappingSHA256) != 64) {
		return errors.New("unsupported datastore manifest")
	}
	if m.Scope == "workload" {
		if len(m.Members) == 0 || len(m.Members) != len(m.Inventory.Snapshots) {
			return errors.New("incomplete workload member set")
		}
		seen := map[string]bool{}
		for _, member := range m.Members {
			if !identifier.MatchString(member.SnapshotHandle) || !identifier.MatchString(member.VolumeHandle) || !identifier.MatchString(member.SnapshotUID) || !identifier.MatchString(member.SnapshotContentUID) || seen[member.SnapshotHandle] {
				return errors.New("invalid workload members")
			}
			seen[member.SnapshotHandle] = true
		}
		for _, id := range m.Inventory.Snapshots {
			if !seen[id] {
				return errors.New("workload member/capture mismatch")
			}
		}
	}
	for _, ids := range [][]string{m.Inventory.Volumes, m.Inventory.Snapshots} {
		prev := ""
		for _, id := range ids {
			if !identifier.MatchString(id) || id <= prev {
				return errors.New("invalid/duplicate/unsorted pool identity")
			}
			prev = id
		}
	}
	if !m.Inventory.PoolPresent && (len(m.Inventory.Volumes) > 0 || len(m.Inventory.Snapshots) > 0 || m.Inventory.StateSHA256 != "") {
		return errors.New("pool inventory is inconsistent")
	}
	if !reflect.DeepEqual(captures(m.Scope, m.Inventory), m.Captures) {
		return errors.New("incomplete subvolume capture set")
	}
	return nil
}

func (e *Engine) inventory(ctx context.Context) (Inventory, error) {
	in := Inventory{Volumes: []string{}, Snapshots: []string{}}
	st, err := os.Lstat(e.cfg.PoolDir)
	if os.IsNotExist(err) {
		return in, nil
	}
	if err != nil {
		return in, err
	}
	if !st.IsDir() || !e.cfg.FS.IsSubvolume(ctx, e.cfg.PoolDir) {
		return in, errors.New("CSI pool is not a subvolume")
	}
	in.PoolPresent = true
	for _, kind := range []string{"volumes", "snapshots"} {
		es, err := os.ReadDir(filepath.Join(e.cfg.PoolDir, kind))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return in, err
		}
		for _, ent := range es {
			if !identifier.MatchString(ent.Name()) || !ent.IsDir() || !e.cfg.FS.IsSubvolume(ctx, filepath.Join(e.cfg.PoolDir, kind, ent.Name())) {
				return in, fmt.Errorf("unexpected CSI pool entry %s/%s", kind, ent.Name())
			}
			if kind == "volumes" {
				in.Volumes = append(in.Volumes, ent.Name())
			} else {
				in.Snapshots = append(in.Snapshots, ent.Name())
			}
		}
	}
	s, b, err := readDriver(filepath.Join(e.cfg.PoolDir, "state.json"))
	if os.IsNotExist(err) && len(in.Volumes) == 0 && len(in.Snapshots) == 0 {
		return in, nil
	}
	if err != nil {
		return in, fmt.Errorf("CSI driver state: %w", err)
	}
	if err := validateDriver(s, in); err != nil {
		return in, err
	}
	in.StateSHA256 = sum(b)
	return in, nil
}

func (e *Engine) capture(ctx context.Context, id, scope string) error {
	if err := e.cfg.FS.ValidateLayout(ctx, e.cfg.VarLib, e.cfg.PoolDir); err != nil {
		return err
	}
	in, err := e.inventory(ctx)
	if err != nil {
		return err
	}
	ds, err := e.cfg.Datastore.Inspect(ctx, e.cfg.VarLib, e.cfg.K3s)
	if err != nil {
		return err
	}
	if ds.Kind != "sqlite" {
		return errors.New("etcd capture is unsupported")
	}
	clean := false
	if _, err := os.Lstat(filepath.Join(e.cfg.VarLib, "containers-anywhere/session")); os.IsNotExist(err) {
		clean = true
	} else if err != nil {
		return err
	}
	m := Manifest{Version: Version, ID: id, Scope: scope, CreatedAt: e.cfg.Now().UTC(), Appliance: e.cfg.Appliance, Architecture: e.cfg.Architecture, DataFormat: e.cfg.DataFormat, Datastore: ds, Inventory: in, Consistency: "cold-crash-consistent", GracefulShutdown: clean, Captures: captures(scope, in)}
	if clean {
		m.Consistency = "cold-after-graceful-shutdown"
	}
	stage, err := os.MkdirTemp(filepath.Join(e.cfg.Root, "staging"), id+"-")
	if err != nil {
		return err
	}
	// A failed stage is retained, never mistaken for a published point.
	for _, name := range m.Captures {
		src := e.cfg.VarLib
		switch {
		case name == "pool":
			src = e.cfg.PoolDir
		case strings.HasPrefix(name, "vol-"):
			src = filepath.Join(e.cfg.PoolDir, "volumes", strings.TrimPrefix(name, "vol-"))
		case strings.HasPrefix(name, "snap-"):
			src = filepath.Join(e.cfg.PoolDir, "snapshots", strings.TrimPrefix(name, "snap-"))
		}
		if err := e.cfg.FS.Snapshot(ctx, src, filepath.Join(stage, name), true); err != nil {
			return err
		}
	}
	if err := e.verify(ctx, stage, m); err != nil {
		return err
	}
	return e.publish(ctx, stage, m)
}

func (e *Engine) publish(ctx context.Context, stage string, m Manifest) error {
	if err := e.cfg.FS.Sync(ctx, stage); err != nil {
		return err
	}
	if err := writeRecord(filepath.Join(stage, "manifest.json"), m); err != nil {
		return err
	}
	if _, err := os.Lstat(e.pointPath(m.ID)); !os.IsNotExist(err) {
		return errors.New("point already exists or cannot be inspected")
	}
	if err := os.Rename(stage, e.pointPath(m.ID)); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(stage)); err != nil {
		return err
	}
	return syncDir(filepath.Join(e.cfg.Root, "points"))
}

func (e *Engine) verify(ctx context.Context, dir string, m Manifest) error {
	if err := e.validateManifest(m); err != nil {
		return err
	}
	for _, name := range m.Captures {
		st, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return errors.New("captured subvolume is not a real directory")
		}
		if !e.cfg.FS.IsSubvolume(ctx, filepath.Join(dir, name)) {
			return fmt.Errorf("missing captured subvolume %s", name)
		}
		ro, err := e.cfg.FS.ReadOnly(ctx, filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !ro {
			return errors.New("captured subvolume is not read-only")
		}
	}
	if m.Scope == "workload" {
		return nil
	}
	var marker struct {
		Contract string `json:"contract"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "system/containers-anywhere/data-format.json"))
	if err != nil {
		return err
	}
	if json.Unmarshal(b, &marker) != nil || marker.Contract != m.DataFormat {
		return errors.New("snapshot data format mismatch")
	}
	ds, err := e.cfg.Datastore.Inspect(ctx, filepath.Join(dir, "system"), m.Datastore.K3s)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(ds, m.Datastore) {
		return errors.New("datastore/identity digest mismatch")
	}
	if m.Scope == "full" && m.Inventory.PoolPresent {
		s, b, err := readDriver(filepath.Join(dir, "pool/state.json"))
		if m.Inventory.StateSHA256 == "" && os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if sum(b) != m.Inventory.StateSHA256 {
			return errors.New("CSI driver state digest mismatch")
		}
		if err := validateDriver(s, m.Inventory); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) mappingMatches(ctx context.Context, m Manifest) error {
	in, err := e.inventory(ctx)
	if err != nil {
		return err
	}
	ds, err := e.cfg.Datastore.Inspect(ctx, e.cfg.VarLib, e.cfg.K3s)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(in, m.Inventory) || ds.MappingSHA256 != m.Datastore.MappingSHA256 {
		return errors.New("system-only restore refused: current PV/PVC/CSI snapshot mappings or volume inventory differ; use a full point")
	}
	return nil
}

func (e *Engine) restore(ctx context.Context, m Manifest) error {
	if err := e.compatible(m); err != nil {
		return err
	}
	if err := e.verify(ctx, e.pointPath(m.ID), m); err != nil {
		return err
	}
	// mappingMatches is intentionally only before the destructive journal phase:
	// after a crash the live datastore may be only partly restored.
	if err := storage.ReplaceSystem(ctx, e.cfg.FS, e.cfg.VarLib, e.cfg.PoolDir, filepath.Join(e.pointPath(m.ID), "system"), e.cfg.Copy); err != nil {
		return err
	}
	if m.Scope == "full" {
		if err := storage.RemoveTree(ctx, e.cfg.FS, e.cfg.PoolDir); err != nil && !os.IsNotExist(err) {
			return err
		}
		if m.Inventory.PoolPresent {
			if err := e.cfg.FS.Snapshot(ctx, filepath.Join(e.pointPath(m.ID), "pool"), e.cfg.PoolDir, false); err != nil {
				return err
			}
			for _, kind := range []string{"volumes", "snapshots"} {
				ids := m.Inventory.Volumes
				prefix := "vol-"
				if kind == "snapshots" {
					ids = m.Inventory.Snapshots
					prefix = "snap-"
				}
				if err := os.MkdirAll(filepath.Join(e.cfg.PoolDir, kind), 0700); err != nil {
					return err
				}
				for _, id := range ids {
					dst := filepath.Join(e.cfg.PoolDir, kind, id)
					if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
						return err
					}
					if err := e.cfg.FS.Snapshot(ctx, filepath.Join(e.pointPath(m.ID), prefix+id), dst, kind == "snapshots"); err != nil {
						return err
					}
				}
			}
		}
		if m.Inventory.StateSHA256 != "" {
			s, _, err := readDriver(filepath.Join(e.cfg.PoolDir, "state.json"))
			if err != nil {
				return err
			}
			for _, id := range m.Inventory.Volumes {
				if err := e.cfg.FS.SetLimit(ctx, filepath.Join(e.cfg.PoolDir, "volumes", id), s.Volumes[id].CapacityBytes); err != nil {
					return err
				}
			}
		}
	}
	if err := storage.ResetImageCache(e.cfg.VarLib); err != nil {
		return err
	}
	return e.cfg.FS.Sync(ctx, e.cfg.VarLib)
}

func (e *Engine) Delete(ctx context.Context, id string) error {
	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := e.get(id); err != nil {
		return err
	}
	ops, err := e.operations()
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Request.Action == "restore" && (op.PointID == id || op.SafetyID == id) {
			return errors.New("restore source and safety points are retained")
		}
		if active(op) && op.PointID == id {
			return ErrBusy
		}
	}
	ps, err := e.populations()
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Request.PointID == id {
			return errors.New("workload restore source points are retained")
		}
	}
	// Unpublish first; interrupted deletion cannot leave a listed partial point.
	tomb, err := os.MkdirTemp(filepath.Join(e.cfg.Root, "staging"), "delete-")
	if err != nil {
		return err
	}
	if err := syncDir(filepath.Join(e.cfg.Root, "staging")); err != nil {
		return err
	}
	if err := os.Rename(e.pointPath(id), filepath.Join(tomb, "point")); err != nil {
		return err
	}
	if err := syncDir(filepath.Join(e.cfg.Root, "points")); err != nil {
		return err
	}
	if err := syncDir(tomb); err != nil {
		return err
	}
	return storage.RemoveTree(ctx, e.cfg.FS, tomb)
}
