package guest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/protocol"
	"github.com/tellmeY18/containers_anywhere/internal/recovery"
	"github.com/tellmeY18/containers_anywhere/internal/storage"
)

// copyTree copies the contents of src into dst keeping ownership, modes, links and extended
// attributes. On btrfs it clones extents, so it is cheap.
func copyTree(ctx context.Context, src, dst string) error {
	out, err := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", src+"/.", dst+"/").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// Old-format pending journals cannot prove a complete CSI snapshot capture set.
// Preserve them and fail closed rather than activate an ambiguous partial restore.
func applyPendingRestore(varLib string) error {
	_, err := os.Lstat(filepath.Join(snapshotsDir, "pending-restore.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("unsupported old restore journal preserved in %s; use its matching appliance or a new state directory", snapshotsDir)
}

// restoreRoutes lets the launcher list restore points and ask for one. Asking changes nothing
// by itself: the restore runs at the next start, before the cluster does.
func (g *guestHostAPI) restoreRoutes(mux *http.ServeMux) {
	version := func() string { return os.Getenv("CA_APPLIANCE_VERSION") }
	mux.HandleFunc("GET /_agent/storage/restore-points", func(w http.ResponseWriter, r *http.Request) {
		e, err := recoveryEngine("/var/lib", false)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		catalog, err := e.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		points := []storage.RestorePoint{}
		for _, m := range catalog {
			if m.Scope == "workload" {
				continue
			}
			compatible := m.Appliance == version() && m.Datastore.K3s == k3sVersion() && m.Architecture == runtime.GOARCH && m.DataFormat == protocol.DataContract
			reason := ""
			if !compatible {
				reason = "requires the exact appliance, K3s, architecture and data format recorded by this point"
			}
			points = append(points, storage.RestorePoint{ID: m.ID, From: m.Appliance, To: m.Appliance, K3s: m.Datastore.K3s, Label: m.Scope, CreatedAt: m.CreatedAt.Format(time.RFC3339), Volumes: m.Inventory.Volumes, Restorable: compatible, Reason: reason})
		}
		ops, err := e.Operations()
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		pending := ""
		var last storage.RestoreResult
		var lastTime time.Time
		for _, op := range ops {
			if op.Phase != "completed" && op.Phase != "failed" {
				pending = op.PointID
			}
			if op.Request.Action == "restore" && (op.Phase == "completed" || op.Phase == "failed") && op.UpdatedAt.After(lastTime) {
				lastTime = op.UpdatedAt
				last = storage.RestoreResult{ID: op.PointID, OK: op.Phase == "completed", Error: op.Error, Safety: op.SafetyID, CompletedAt: op.UpdatedAt.Format(time.RFC3339)}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"points": points, "pending": pending, "last": last, "operations": ops, "applianceVersion": version()})
	})
	mux.HandleFunc("POST /_agent/storage/restore-point", func(w http.ResponseWriter, r *http.Request) {
		e, err := recoveryEngine("/var/lib", false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		uid := r.Header.Get("Idempotency-Key")
		if uid == "" {
			uid = "manual-" + rand.Text()
		}
		op, err := e.Submit(r.Context(), recovery.Request{UID: uid, Action: "capture", Scope: "full"})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(op)
	})
	mux.HandleFunc("POST /_agent/storage/restore", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		e, err := recoveryEngine("/var/lib", false)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		m, err := e.Get(body.ID)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		uid := r.Header.Get("Idempotency-Key")
		if uid == "" {
			uid = "manual-restore-" + rand.Text()
		}
		if _, err := e.Submit(r.Context(), recovery.Request{UID: uid, Action: "restore", Scope: m.Scope, PointID: m.ID}); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Write([]byte("Restore recorded; it happens when the appliance next starts\n"))
	})
}
