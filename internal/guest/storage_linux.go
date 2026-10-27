package guest

import (
	"context"
	"fmt"
	"os"
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
}
