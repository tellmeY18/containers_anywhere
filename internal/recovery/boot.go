package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type bootIdentity struct {
	Appliance string    `json:"appliance"`
	K3s       string    `json:"k3s"`
	At        time.Time `json:"at"`
}

// CaptureOnChange preserves the previous writer's exact identity, before K3s
// starts under a new appliance. Capture failure is recorded as terminal failed
// while the unchanged source can boot. Failure to durably record that outcome
// still stops boot, so a later boot cannot mislabel the previous writer.
func (e *Engine) CaptureOnChange(ctx context.Context) error {
	if e.cfg.Cold == nil {
		return errors.New("version-change capture requires cold init-data")
	}
	if err := e.cfg.Cold(ctx); err != nil {
		return err
	}
	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(e.cfg.Root, "boot.json")
	var previous bootIdentity
	if err := readRecord(path, &previous); err != nil && !os.IsNotExist(err) {
		return err
	}
	if previous.Appliance == e.cfg.Appliance && previous.K3s == e.cfg.K3s {
		return nil
	}
	if previous.Appliance != "" {
		id := "boot-" + sum([]byte(previous.Appliance+"\x00"+previous.K3s+"\x00"+previous.At.String()+"\x00"+e.cfg.Appliance+"\x00"+e.cfg.K3s))
		old := *e
		old.cfg.Appliance = previous.Appliance
		old.cfg.K3s = previous.K3s
		var captureErr error
		if m, err := e.get(id); os.IsNotExist(err) {
			captureErr = old.capture(ctx, id, "full")
		} else if err != nil {
			captureErr = err
		} else if err := old.verify(ctx, e.pointPath(id), m); err != nil {
			captureErr = err
		}
		op := Operation{Version: Version, Request: Request{UID: id, ID: id, Action: "capture", Scope: "full"}, PointID: id, Phase: "completed", CreatedAt: e.cfg.Now().UTC()}
		if captureErr != nil {
			op.Phase = "failed"
			op.Error = captureErr.Error()
		}
		if err := e.save(&op); err != nil {
			return err
		}
	}
	return writeRecord(path, bootIdentity{e.cfg.Appliance, e.cfg.K3s, e.cfg.Now().UTC()})
}
