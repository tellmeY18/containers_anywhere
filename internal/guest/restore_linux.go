package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

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

func restoreConfig(varLib string) storage.RestoreConfig {
	return storage.RestoreConfig{
		RestorePointConfig: storage.RestorePointConfig{VarLib: varLib, SnapshotsDir: snapshotsDir, PoolDir: poolDir,
			Version: os.Getenv("CA_APPLIANCE_VERSION"), Keep: 2},
		Copy: copyTree,
	}
}

// applyPendingRestore returns an error only when the disk was left inconsistent. A refused or
// rolled-back restore is reported on the console and in the last-restore record, and the
// appliance starts normally on the state that is there.
func applyPendingRestore(varLib string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	done, err := storage.ApplyPendingRestore(ctx, storage.NewBtrfsFS(), restoreConfig(varLib))
	switch {
	case errors.Is(err, storage.ErrStateInconsistent):
		return err
	case err != nil:
		fmt.Fprintln(os.Stderr, "restore:", err)
	case done:
		fmt.Fprintln(os.Stdout, "the requested restore completed")
	}
	return nil
}

// restoreRoutes lets the launcher list restore points and ask for one. Asking changes nothing
// by itself: the restore runs at the next start, before the cluster does.
func (g *guestHostAPI) restoreRoutes(mux *http.ServeMux) {
	version := func() string { return os.Getenv("CA_APPLIANCE_VERSION") }
	mux.HandleFunc("GET /_agent/storage/restore-points", func(w http.ResponseWriter, r *http.Request) {
		points, err := storage.ListRestorePoints(snapshotsDir, version())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		last, _ := storage.LastRestore(snapshotsDir)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"points": points, "pending": storage.PendingRestore(snapshotsDir), "last": last, "applianceVersion": version()})
	})
	mux.HandleFunc("POST /_agent/storage/restore", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := storage.RequestRestore(snapshotsDir, body.ID, version(), time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Write([]byte("Restore recorded; it happens when the appliance next starts\n"))
	})
}
