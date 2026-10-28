package guest

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/hostapi"
	"github.com/tellmeY18/containers_anywhere/internal/publication"
)

const (
	varLibRoot        = "/var/lib"
	certsDir          = "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"
	registryStateFile = "/var/lib/containers-anywhere/registry-trust.json"
	journalDir        = "/var/lib/containers-anywhere/journal"
	journalCapBytes   = 64 << 20
)

// guestHostAPI runs the generic host bridge inside the guest agent. It is the
// only component that talks to the Kubernetes API on the host's behalf.
type guestHostAPI struct {
	hostapi.Controller

	pruneMu sync.Mutex

	storageMu sync.Mutex
	storage   *hostapi.StorageUse
	storageAt time.Time
}

func newGuestHostAPI() *guestHostAPI {
	g := &guestHostAPI{}
	g.Run = publication.Kubectl
	return g
}

func readUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int64(v)
}

func dataDisk() hostapi.Disk {
	var st syscall.Statfs_t
	if err := syscall.Statfs(varLibRoot, &st); err != nil {
		return hostapi.Disk{}
	}
	bs := int64(st.Bsize)
	return hostapi.DiskFromStatfs(int64(st.Blocks)*bs, int64(st.Bavail)*bs)
}

func localAddresses() []net.IP {
	var out []net.IP
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out
}

// storageUse measures large directories at most every five minutes, each under a
// time budget, because a walk of the image store is not free.
func (g *guestHostAPI) storageUse(ctx context.Context) *hostapi.StorageUse {
	g.storageMu.Lock()
	defer g.storageMu.Unlock()
	if g.storage != nil && time.Since(g.storageAt) < 5*time.Minute {
		return g.storage
	}
	measure := func(path string) (int64, bool) { return hostapi.DirSize(ctx, path, 15*time.Second) }
	images, c1 := measure("/var/lib/rancher/k3s/agent/containerd")
	volumes, c2 := measure("/var/lib/rancher/k3s/storage")
	console, c3 := measure("/var/lib/containers-anywhere/kite")
	journal, c4 := measure(journalDir)
	g.storage = &hostapi.StorageUse{ImagesBytes: images, VolumesBytes: volumes, ConsoleBytes: console, JournalBytes: journal,
		MeasuredAt: time.Now().UTC().Format(time.RFC3339), Partial: !(c1 && c2 && c3 && c4)}
	g.storageAt = time.Now()
	return g.storage
}

func journalSummary(ctx context.Context, bytes int64) *hostapi.Logs {
	logs := &hostapi.Logs{Bytes: bytes, CapBytes: journalCapBytes}
	if info, err := os.Stat(journalDir); err == nil && info.IsDir() {
		logs.Persisted = true
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "journalctl", "--list-boots", "--no-pager").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(line) != "" {
				logs.Boots++
			}
		}
	}
	return logs
}

func (g *guestHostAPI) guestFacts(ctx context.Context) hostapi.GuestFacts {
	storage := g.storageUse(ctx)
	return hostapi.GuestFacts{UptimeSeconds: readUptime(), Disk: dataDisk(), Storage: storage, Logs: journalSummary(ctx, storage.JournalBytes)}
}

func crdsMissing(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "doesn't have a resource type") || strings.Contains(m, "no matches for kind") || strings.Contains(m, "the server could not find the requested resource")
}

// run reconciles every few seconds. The CRDs are shipped by the app, so before
// the first push they do not exist; that is reported, not treated as a failure.
func (g *guestHostAPI) run(ctx context.Context) {
	var lastLog time.Time
	logOnce := func(what string, err error) {
		if err != nil && time.Since(lastLog) > time.Minute {
			log.Printf("host API %s: %v", what, err)
			lastLog = time.Now()
		}
	}
	for {
		step, cancel := context.WithTimeout(ctx, 40*time.Second)
		err := g.ReconcileStatus(step, g.guestFacts(step))
		g.SetCRDsMissing(crdsMissing(err))
		if err == nil {
			logOnce("requests", g.ReconcileRequests(step))
			logOnce("destinations", g.ReconcileDestinations(step))
			logOnce("recovery points", g.ReconcileRecoveryPoints(step))
			logOnce("registry trust", g.ReconcileRegistryTrust(step, certsDir, registryStateFile, localAddresses()))
		} else if !crdsMissing(err) {
			logOnce("status", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func crictl(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "k3s", append([]string{"crictl"}, args...)...).Output()
}

func imageBytes(ctx context.Context) (int64, error) {
	out, err := crictl(ctx, "images", "-o", "json")
	if err != nil {
		return 0, err
	}
	return hostapi.ImageBytes(out)
}

// pruneImages removes unreferenced images only. Volumes and images in use are
// never touched: crictl rmi --prune skips images that any container references.
func (g *guestHostAPI) pruneImages(ctx context.Context) (hostapi.PruneResult, error) {
	g.pruneMu.Lock()
	defer g.pruneMu.Unlock()
	before, err := imageBytes(ctx)
	if err != nil {
		return hostapi.PruneResult{}, err
	}
	if _, err := crictl(ctx, "rmi", "--prune"); err != nil {
		return hostapi.PruneResult{}, err
	}
	after, err := imageBytes(ctx)
	if err != nil {
		return hostapi.PruneResult{}, err
	}
	g.storageMu.Lock()
	g.storage = nil // re-measure next cycle
	g.storageMu.Unlock()
	return hostapi.NewPruneResult(before, after), nil
}

func (g *guestHostAPI) routes(mux *http.ServeMux) {
	g.Routes(mux)
	mux.HandleFunc("POST /_agent/host/prune-images", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		result, err := g.pruneImages(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})
}
