package guest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/protocol"
	"github.com/tellmeY18/containers_anywhere/internal/recovery"
)

func recoveryEngine(varLib string, cold bool) (*recovery.Engine, error) {
	probe, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !recovery.NewBtrfsBackend().IsSubvolume(probe, snapshotsDir) {
		return nil, errors.New("durable @snapshots mount is unavailable")
	}
	cfg := recovery.Config{Root: filepath.Join(snapshotsDir, "recovery"), VarLib: varLib, PoolDir: filepath.Join(varLib, "btrfs-csi-pool"), Appliance: os.Getenv("CA_APPLIANCE_VERSION"), Architecture: runtime.GOARCH, DataFormat: protocol.DataContract, K3s: k3sVersion(), FS: recovery.NewBtrfsBackend(), Datastore: recovery.SQLiteAdapter{}, Copy: copyTree}
	if cold {
		cfg.Cold = func(ctx context.Context) error {
			// init-data is ordered before K3s by systemd. Also refuse direct accidental
			// invocation while a server/container runtime is still alive.
			entries, err := os.ReadDir("/proc")
			if err != nil {
				return err
			}
			for _, ent := range entries {
				if !ent.IsDir() {
					continue
				}
				b, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "comm"))
				if err != nil {
					continue
				}
				name := strings.TrimSpace(string(b))
				if name == "k3s" || name == "containerd" || name == "kubelet" || strings.HasPrefix(name, "containerd-shim") {
					return errors.New("cold recovery refused: K3s/containerd is running")
				}
			}
			return ctx.Err()
		}
	}
	return recovery.New(cfg)
}

// The parent launcher/controller polls restart-needed and restarts the entire
// appliance. Nothing in this handler stops K3s from inside its own workload.
func (g *guestHostAPI) recoveryRoutes(mux *http.ServeMux) {
	e, err := recoveryEngine("/var/lib", false)
	if err != nil {
		mux.HandleFunc("/_agent/recovery/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, fmt.Sprintf("recovery unavailable: %v", err), 503)
		})
		return
	}
	e.Routes(mux)
}

func applyPendingRecovery(varLib string) error {
	e, err := recoveryEngine(varLib, true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if err := e.ProcessCold(ctx); err != nil {
		return err
	}
	return e.CaptureOnChange(ctx)
}
