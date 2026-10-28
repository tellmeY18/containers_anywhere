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
	// The mount roots must be root-owned before systemd-tmpfiles runs, or it refuses to
	// create anything beneath them.
	if err := storage.EnsureRootOwned(varLib, snapshotsDir); err != nil {
		fmt.Fprintln(os.Stderr, "mount root ownership:", err)
	}
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
	linkJournal(varLib)
	if unclean, err := storage.BeginSession(varLib); err != nil {
		fmt.Fprintln(os.Stderr, "session marker:", err)
	} else if unclean {
		fmt.Fprintln(os.Stdout, "the previous session did not end cleanly: discarding the container image cache (volumes are untouched)")
		if err := storage.ResetImageCache(varLib); err != nil {
			fmt.Fprintln(os.Stderr, "image cache reset:", err)
		}
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
}

// linkJournal points /var/log/journal at the journal on the data disk before the
// journal is flushed, so the guest's logs persist without tmpfiles having to run first.
func linkJournal(varLib string) {
	target := filepath.Join(varLib, "containers-anywhere", "journal")
	if _, err := os.Lstat("/var/log/journal"); err == nil {
		return
	}
	if err := os.MkdirAll("/var/log", 0o755); err == nil {
		if err := os.Symlink(target, "/var/log/journal"); err != nil {
			fmt.Fprintln(os.Stderr, "journal link:", err)
		}
	}
}

// endSession runs when the appliance shuts down, after the cluster has stopped.
func endSession(varLib string) error { return storage.EndSession(varLib) }
