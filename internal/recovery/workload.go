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
)

type SnapshotMember struct {
	PVCNamespace       string `json:"pvcNamespace"`
	PVCName            string `json:"pvcName"`
	PVCUID             string `json:"pvcUID"`
	PVName             string `json:"pvName"`
	VolumeHandle       string `json:"volumeHandle"`
	SnapshotHandle     string `json:"snapshotHandle"`
	SnapshotUID        string `json:"snapshotUID"` // VolumeSnapshot UID used by external-snapshotter's CSI name
	SnapshotContentUID string `json:"snapshotContentUID"`
	Driver             string `json:"driver"`
	// Metadata holds the controller's exact completed member/resource metadata.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}
type RegisterRequest struct {
	ID          string           `json:"id"`
	Consistency string           `json:"consistency,omitempty"`
	Members     []SnapshotMember `json:"members"`
}

// Driver state is the pinned mporrato/btrfs-csi file-store contract. Paths are
// never accepted from callers or from serialized BasePath fields.
type driverVolume struct {
	ID, Name, SourceSnapID, SourceVolID string
	CapacityBytes                       int64
	Nodatacow                           bool
}
type driverSnapshot struct {
	ID, Name, SourceVolID string
	ReadyToUse            bool
}
type driverState struct {
	Volumes   map[string]*driverVolume   `json:"volumes"`
	Snapshots map[string]*driverSnapshot `json:"snapshots"`
}

func readDriver(path string) (driverState, []byte, error) {
	var s driverState
	st, err := os.Lstat(path)
	if err != nil {
		return s, nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 10<<20 {
		return s, nil, errors.New("invalid CSI state file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, nil, err
	}
	err = json.Unmarshal(b, &s)
	return s, b, err
}

func validateDriver(s driverState, in Inventory) error {
	if len(s.Volumes) != len(in.Volumes) || len(s.Snapshots) != len(in.Snapshots) {
		return errors.New("CSI state/subvolume inventory mismatch")
	}
	for _, id := range in.Volumes {
		v := s.Volumes[id]
		if v == nil || v.ID != id || v.CapacityBytes <= 0 {
			return fmt.Errorf("invalid CSI volume %s", id)
		}
	}
	for _, id := range in.Snapshots {
		v := s.Snapshots[id]
		if v == nil || v.ID != id || !v.ReadyToUse || !identifier.MatchString(v.SourceVolID) {
			return fmt.Errorf("invalid CSI snapshot %s", id)
		}
	}
	return nil
}

// RegisterSnapshots retains already-completed CSI snapshots without restarting
// K3s. It promises only each individual snapshot's crash consistency, never an
// atomic multi-volume or application-consistent checkpoint. Kubernetes ownership
// and completed resource metadata are established by the authenticated controller.
func (e *Engine) RegisterSnapshots(ctx context.Context, req RegisterRequest) (Manifest, error) {
	if req.Consistency == "" {
		req.Consistency = "individual-csi-crash-consistent"
	}
	if req.Consistency != "individual-csi-crash-consistent" && req.Consistency != "quiesced-group-hook-contract" {
		return Manifest{}, errors.New("unsupported workload capture consistency")
	}
	if !identifier.MatchString(req.ID) || len(req.Members) == 0 || len(req.Members) > MaxMembers {
		return Manifest{}, errors.New("invalid workload snapshot registration")
	}
	unlock, err := e.lock()
	if err != nil {
		return Manifest{}, err
	}
	defer unlock()
	// Do not race a queued cold restore, even though no live system state is written.
	ops, err := e.operations()
	if err != nil {
		return Manifest{}, err
	}
	for _, op := range ops {
		if op.Request.UID == req.ID {
			return Manifest{}, errors.New("UID already used by a system/full operation")
		}
		if active(op) {
			return Manifest{}, ErrBusy
		}
	}
	id := key(req.ID)
	if old, err := e.get(id); err == nil {
		if old.Scope != "workload" || old.Consistency != req.Consistency || !reflect.DeepEqual(old.Members, req.Members) {
			return old, errors.New("UID already registered with different members")
		}
		return old, nil
	} else if !os.IsNotExist(err) {
		return old, err
	}
	s, _, err := readDriver(filepath.Join(e.cfg.PoolDir, "state.json"))
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Version: Version, ID: id, Scope: "workload", CreatedAt: e.cfg.Now().UTC(), Appliance: e.cfg.Appliance, Architecture: e.cfg.Architecture, DataFormat: e.cfg.DataFormat, Consistency: req.Consistency, Members: req.Members, Inventory: Inventory{PoolPresent: true, Volumes: []string{}, Snapshots: []string{}}}
	seen := map[string]bool{}
	for _, member := range req.Members {
		v := s.Volumes[member.VolumeHandle]
		snap := s.Snapshots[member.SnapshotHandle]
		if !identifier.MatchString(member.VolumeHandle) || !identifier.MatchString(member.SnapshotHandle) || !identifier.MatchString(member.PVCUID) || !identifier.MatchString(member.SnapshotUID) || !identifier.MatchString(member.SnapshotContentUID) || member.PVCNamespace == "" || member.PVCName == "" || member.PVName == "" || member.Driver != "btrfs.csi.local" || seen[member.SnapshotHandle] {
			return m, errors.New("invalid or duplicate workload member identity")
		}
		if v == nil || v.ID != member.VolumeHandle || v.Name != "pvc-"+member.PVCUID || snap == nil || snap.ID != member.SnapshotHandle || snap.SourceVolID != v.ID || snap.Name != "snapshot-"+member.SnapshotUID || !snap.ReadyToUse {
			return m, errors.New("workload member does not match actual completed CSI driver state")
		}
		seen[member.SnapshotHandle] = true
		m.Inventory.Snapshots = append(m.Inventory.Snapshots, member.SnapshotHandle)
	}
	sort.Strings(m.Inventory.Snapshots)
	m.Captures = captures(m.Scope, m.Inventory)
	stage, err := os.MkdirTemp(filepath.Join(e.cfg.Root, "staging"), id+"-")
	if err != nil {
		return m, err
	}
	for _, sid := range m.Inventory.Snapshots {
		src := filepath.Join(e.cfg.PoolDir, "snapshots", sid)
		st, err := os.Lstat(src)
		if err != nil {
			return m, err
		}
		if !st.IsDir() {
			return m, errors.New("CSI snapshot is not a real directory")
		}
		ro, err := e.cfg.FS.ReadOnly(ctx, src)
		if err != nil {
			return m, err
		}
		if !ro {
			return m, errors.New("CSI snapshot is not read-only")
		}
		if err := e.cfg.FS.Snapshot(ctx, src, filepath.Join(stage, "snap-"+sid), true); err != nil {
			return m, err
		}
	}
	// Detect driver changes/deletion during acquisition; already retained copies
	// remain private staging data on failure.
	after, _, err := readDriver(filepath.Join(e.cfg.PoolDir, "state.json"))
	if err != nil {
		return m, err
	}
	for _, member := range req.Members {
		if !reflect.DeepEqual(s.Volumes[member.VolumeHandle], after.Volumes[member.VolumeHandle]) || !reflect.DeepEqual(s.Snapshots[member.SnapshotHandle], after.Snapshots[member.SnapshotHandle]) {
			return m, errors.New("CSI identities changed during snapshot registration")
		}
	}
	if err := e.verify(ctx, stage, m); err != nil {
		return m, err
	}
	return m, e.publish(ctx, stage, m)
}
