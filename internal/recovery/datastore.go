package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SQLiteAdapter requires the appliance sqlite3 binary. It inspects only cold
// data or immutable snapshots, including any WAL. No live SQLite copy is made.
type SQLiteAdapter struct{}

func (SQLiteAdapter) Inspect(ctx context.Context, root, k3s string) (Datastore, error) {
	d := Datastore{Kind: "sqlite", AdapterVersion: 1, K3s: k3s, Files: map[string]string{}}
	if _, err := os.Lstat(filepath.Join(root, "rancher/k3s/server/db/etcd")); !os.IsNotExist(err) {
		return d, errors.New("etcd capture/restore is not implemented")
	}
	for _, rel := range []string{"rancher/k3s/server/db/state.db", "rancher/k3s/server/token", "rancher/node/password"} {
		st, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			return d, err
		}
		if st.Size() == 0 {
			return d, fmt.Errorf("empty required datastore/identity %s", rel)
		}
		h, err := hashRegular(filepath.Join(root, rel))
		if err != nil {
			return d, fmt.Errorf("required datastore/identity %s: %w", rel, err)
		}
		d.Files[rel] = h
	}
	for _, rel := range []string{"rancher/k3s/server/db/state.db-wal", "rancher/k3s/server/db/state.db-journal", "rancher/k3s/server/cred/encryption-config.json", "rancher/k3s/server/cred/encryption-state.json"} {
		h, err := hashRegular(filepath.Join(root, rel))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return d, err
		}
		d.Files[rel] = h
	}
	for _, rel := range []string{"rancher/k3s/server/tls", "rancher/k3s/server/cred"} {
		err := filepath.WalkDir(filepath.Join(root, rel), func(p string, ent os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ent.IsDir() {
				return nil
			}
			r, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			h, err := hashRegular(p)
			if err != nil {
				return err
			}
			d.Files[filepath.ToSlash(r)] = h
			return nil
		})
		if err != nil {
			return d, fmt.Errorf("K3s identity tree: %w", err)
		}
	}
	if len(d.Files) < 4 {
		return d, errors.New("incomplete K3s identity")
	}
	db := filepath.Join(root, "rancher/k3s/server/db/state.db")
	out, err := exec.CommandContext(ctx, "sqlite3", "-readonly", db, "PRAGMA quick_check;").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		return d, errors.New("SQLite integrity check failed")
	}
	// Raw stored object bytes conservatively include resource versions. Rejecting
	// an otherwise harmless metadata change is preferable to accepting a changed
	// PV, PVC, VolumeSnapshot or VolumeSnapshotContent binding.
	query := `SELECT hex(name)||':'||hex(value) FROM kine WHERE id IN (SELECT MAX(id) FROM kine GROUP BY name) AND deleted=0 AND (name LIKE '/registry/persistentvolumes/%' OR name LIKE '/registry/persistentvolumeclaims/%' OR name LIKE '/registry/volumesnapshotcontents/%' OR name LIKE '/registry/volumesnapshots/%' OR name LIKE '/registry/snapshot.storage.k8s.io/%') ORDER BY name;`
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", db, query)
	h := sha256.New()
	cmd.Stdout = h
	if err := cmd.Run(); err != nil {
		return d, errors.New("cannot inventory SQLite PV/PVC/snapshot mappings")
	}
	d.MappingSHA256 = hex.EncodeToString(h.Sum(nil))
	return d, nil
}

func hashRegular(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("expected regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
