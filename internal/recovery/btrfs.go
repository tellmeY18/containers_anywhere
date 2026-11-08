package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tellmeY18/containers_anywhere/internal/storage"
)

type BtrfsBackend struct{ *storage.BtrfsFS }

func NewBtrfsBackend() *BtrfsBackend { return &BtrfsBackend{storage.NewBtrfsFS()} }
func run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "btrfs", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("btrfs %s: %w: %s", args[0], err, out)
	}
	return out, nil
}
func (b *BtrfsBackend) Sync(ctx context.Context, path string) error {
	_, err := run(ctx, "filesystem", "sync", path)
	return err
}
func (b *BtrfsBackend) ReadOnly(ctx context.Context, path string) (bool, error) {
	out, err := run(ctx, "property", "get", "-ts", path, "ro")
	return strings.TrimSpace(string(out)) == "ro=true", err
}
func (b *BtrfsBackend) SetLimit(ctx context.Context, path string, size int64) error {
	if size <= 0 {
		return errors.New("invalid volume quota")
	}
	_, err := run(ctx, "qgroup", "limit", fmt.Sprint(size), path)
	return err
}

func (b *BtrfsBackend) Mounted(ctx context.Context, path string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	// Btrfs bind-mount roots identify their filesystem-relative subvolume, even
	// when mounted into kubelet paths. Refuse subpath mounts as well as root ones.
	needle := "/volumes/" + filepath.Base(path)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return false, errors.New("unreadable mount inventory")
		}
		root := fields[3]
		if i := strings.Index(root, needle); i >= 0 && (i+len(needle) == len(root) || root[i+len(needle)] == '/') {
			return true, nil
		}
	}
	return false, nil
}

func (b *BtrfsBackend) ValidateLayout(ctx context.Context, root, pool string) error {
	if !b.IsSubvolume(ctx, root) {
		return errors.New("system is not a Btrfs subvolume")
	}
	// list -o reports filesystem-root-relative paths, even when /var/lib is
	// mounted as @system. Unknown nested subvolumes would silently disappear in
	// a parent snapshot, so fail rather than advertise an incomplete capture.
	out, err := run(ctx, "subvolume", "list", "-o", root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, pool)
	if err != nil {
		return err
	}
	poolRel := "@system/" + filepath.ToSlash(rel)
	if _, err := os.Lstat(pool); err == nil {
		nested, err := run(ctx, "subvolume", "list", "-o", pool)
		if err != nil {
			return err
		}
		out = append(append(out, '\n'), nested...)
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		_, p, ok := strings.Cut(line, " path ")
		if !ok {
			return errors.New("unreadable Btrfs inventory")
		}
		allowed := p == "@system/rancher/k3s/agent/containerd" || p == "@system/containers-anywhere/journal" || p == poolRel
		for _, kind := range []string{"volumes", "snapshots"} {
			prefix := poolRel + "/" + kind + "/"
			if strings.HasPrefix(p, prefix) && identifier.MatchString(strings.TrimPrefix(p, prefix)) {
				allowed = true
				children, err := run(ctx, "subvolume", "list", "-o", filepath.Join(pool, kind, strings.TrimPrefix(p, prefix)))
				if err != nil {
					return err
				}
				if len(bytes.TrimSpace(children)) != 0 {
					return fmt.Errorf("nested CSI volume subvolumes are unsupported: %s", p)
				}
			}
		}
		if !allowed {
			return fmt.Errorf("unsupported nested subvolume %q; cannot capture a complete point", p)
		}
	}
	return nil
}

func (b *BtrfsBackend) Send(ctx context.Context, path string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "btrfs", "send", "--proto", "1", path)
	reader, writer := io.Pipe()
	checked := make(chan error, 1)
	go func() {
		err := validateStream(io.TeeReader(reader, w), filepath.Base(path))
		_ = reader.CloseWithError(err)
		checked <- err
	}()
	cmd.Stdout = writer
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	_ = writer.CloseWithError(err)
	validationErr := <-checked
	if err != nil {
		return fmt.Errorf("btrfs send: %w: %s", err, stderr.String())
	}
	return validationErr
}

func (b *BtrfsBackend) Receive(ctx context.Context, dir, name string, r io.Reader) error {
	if name != "system" && name != "pool" && !((strings.HasPrefix(name, "vol-") && identifier.MatchString(strings.TrimPrefix(name, "vol-"))) || (strings.HasPrefix(name, "snap-") && identifier.MatchString(strings.TrimPrefix(name, "snap-")))) {
		return errors.New("invalid received subvolume name")
	}
	// The engine already authenticated/spooled this member. Keep its guarded
	// reader through validation AND command stdin, without duplicating that spool.
	stream, ok := r.(io.ReadSeeker)
	if !ok {
		return errors.New("Btrfs receive requires a seekable authenticated spool")
	}
	size, err := stream.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if size < 1 || size > MaxStreamBytes {
		return errors.New("Btrfs stream too large")
	}
	if _, err := stream.Seek(0, 0); err != nil {
		return err
	}
	if err := validateStream(stream, name); err != nil {
		return err
	}
	if _, err := stream.Seek(0, 0); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "btrfs", "receive", "--chroot", dir)
	cmd.Stdin = stream
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs receive: %w: %s", err, out)
	}
	if !b.IsSubvolume(ctx, filepath.Join(dir, name)) {
		return errors.New("receive did not create its declared subvolume")
	}
	out, err = run(ctx, "property", "get", "-ts", filepath.Join(dir, name), "ro")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "ro=true" {
		return errors.New("received snapshot is not read-only")
	}
	return b.Sync(ctx, dir)
}
