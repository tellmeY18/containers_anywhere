package guest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/recovery"
	"github.com/tellmeY18/containers_anywhere/internal/storage"
)

const (
	// The CSI driver, run by the app, keeps its pool in this subvolume (it bind-mounts it at
	// /var/lib/btrfs-csi/default). The appliance only needs the path to include it in restore points.
	poolDir      = "/var/lib/btrfs-csi-pool"
	snapshotsDir = "/run/ca/snapshots"
)

// initData validates ordinary live data before initializing recovery or changing
// ownership. Only an already-destructive restore may resume before that check:
// its partial replacement may not contain a live format marker yet.
func initData(varLib string) error {
	if err := applyPendingRestore(varLib); err != nil {
		return err
	}
	resume, err := recovery.NeedsRestoreResume(filepath.Join(snapshotsDir, "recovery"))
	if err != nil {
		return err
	}
	if !resume {
		if err := initDataFormat(varLib); err != nil {
			return err
		}
	}
	if err := applyPendingRecovery(varLib); err != nil {
		return err
	}
	if resume {
		if err := initDataFormat(varLib); err != nil {
			return err
		}
	}
	// Ownership changes also wait for validation. The roots are ready before
	// systemd-tmpfiles starts, including after a resumed restore.
	if err := storage.EnsureRootOwned(varLib, snapshotsDir); err != nil {
		fmt.Fprintln(os.Stderr, "mount root ownership:", err)
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
	return nil
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

// k3sVersion is the Kubernetes version this appliance runs ("1.35.8+k3s1"), recorded on every
// restore point so a later appliance can tell whether it may open that state.
func k3sVersion() string {
	return strings.TrimSpace(os.Getenv("CA_K3S_VERSION"))
}
