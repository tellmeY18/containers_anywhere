package guest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/storage"
)

const (
	volumesDir   = "/var/lib/rancher/k3s/storage"
	snapshotsDir = "/run/ca/snapshots"
)

// initData runs before the cluster starts. The marker check is the only step that
// may refuse to boot; the layout and the restore point report problems on the
// console and let the appliance come up, because the data is intact either way.
func initData(varLib string) error {
	if err := initDataFormat(varLib); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// K3s keeps its node password here and /etc/rancher/node links to it.
	if err := os.MkdirAll(filepath.Join(varLib, "rancher", "node"), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "node directory:", err)
	}
	fs := storage.NewBtrfsFS()
	if err := storage.EnsureLayout(ctx, fs, varLib); err != nil {
		fmt.Fprintln(os.Stderr, "storage layout:", err)
	}
	point, err := storage.TakeRestorePointOnChange(ctx, fs, storage.RestorePointConfig{
		VarLib: varLib, SnapshotsDir: snapshotsDir, VolumesDir: volumesDir,
		Version: os.Getenv("CA_APPLIANCE_VERSION"), Keep: 2,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "restore point:", err)
	} else if point != "" {
		fmt.Fprintln(os.Stdout, "restore point taken:", point)
	}
	return nil
}

// startStorage provisions volumes and serves the snapshot API for the life of the agent.
func startStorage(ctx context.Context) {
	c := storage.NewController(storage.KubectlKube{Kubeconfig: "/etc/rancher/k3s/k3s.yaml"}, storage.NewBtrfsFS(), volumesDir, snapshotsDir)
	c.Log = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	go c.Run(ctx, 4*time.Second)
	go diagnoseLayout()
}

// diagnoseLayout writes the state of the node directory and its symlink to the
// console, so a boot failure there is visible in the diagnostics. Temporary.
func diagnoseLayout() {
	time.Sleep(20 * time.Second)
	f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	for _, p := range []string{"/etc/rancher/node", "/var/lib/rancher/node", "/var/lib/rancher", "/var/lib/rancher/k3s", "/var/lib"} {
		li, lerr := os.Lstat(p)
		si, serr := os.Stat(p)
		fmt.Fprintf(f, "ca-diag %s lstat=%v/%v stat=%v/%v\n", p, modeOf(li), lerr, modeOf(si), serr)
	}
	for _, dir := range []string{"/var/lib", "/var/lib/rancher"} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			fmt.Fprintf(f, "ca-diag ls %s\n", filepath.Join(dir, e.Name()))
		}
	}
}

func modeOf(i os.FileInfo) string {
	if i == nil {
		return "-"
	}
	return i.Mode().String()
}
