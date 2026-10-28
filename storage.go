package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"filippo.io/age"
	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

func atomicWrite(p string, b []byte, mode os.FileMode) error {
	if mode == 0600 {
		return atomicfile.WritePrivate(p, b)
	}
	return atomicfile.Write(p, b, mode)
}
func verifyBundle(dir string) (manifest, error) {
	var m manifest
	b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		return m, e
	}
	if e = json.Unmarshal(b, &m); e != nil {
		return m, e
	}
	if m.Version == "" || m.Arch == "" || !strings.HasPrefix(m.System, "/nix/store/") ||
		path.Dir(m.System) != "/nix/store" || strings.ContainsAny(m.System, " \t\n\r\\") {
		return m, errors.New("invalid manifest")
	}
	if err := validateLayers(m); err != nil {
		return m, err
	}
	names := []string{m.Kernel, m.Initrd, "system.img", "data.img"}
	for _, n := range names {
		if _, ok := m.Files[n]; !ok {
			return m, fmt.Errorf("missing checksum: %s", n)
		}
	}
	for n, want := range m.Files {
		if n == "" || filepath.Base(n) != n || strings.ContainsAny(n, "\\/:") || n == "." || n == ".." {
			return m, errors.New("invalid bundle filename")
		}
		info, e := os.Lstat(filepath.Join(dir, n))
		if e != nil {
			return m, e
		}
		if !info.Mode().IsRegular() {
			return m, errors.New("bundle member must be a regular file")
		}
		f, e := os.Open(filepath.Join(dir, n))
		if e != nil {
			return m, e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return m, e
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return m, fmt.Errorf("checksum mismatch: %s", n)
		}
	}
	return m, nil
}
func copyExclusive(src, dst string) error {
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	info, e := in.Stat()
	if e != nil {
		return e
	}
	space, e := diskspace.Of(filepath.Dir(dst))
	if e != nil {
		return e
	}
	if space.Free < uint64(info.Size())+(256<<20) {
		return errors.New("insufficient disk space for cluster data")
	}
	out, e := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	if e == nil {
		e = out.Sync()
	}
	e = errors.Join(e, out.Close())
	if e != nil {
		os.Remove(dst)
	}
	return e
}
func ensureData(state, bundle string, m manifest) error {
	path := filepath.Join(state, "data.img")
	if _, e := os.Stat(path); e == nil {
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	if _, e := os.Stat(filepath.Join(state, "release.json")); e == nil {
		return errors.New("release exists but cluster disk missing; restore a backup")
	}
	stage, e := os.MkdirTemp(state, "initialize-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	if e := copyExclusive(filepath.Join(bundle, "data.img"), filepath.Join(stage, "data.img")); e != nil {
		return e
	}
	if m.Format == 3 {
		f, e := os.OpenFile(filepath.Join(stage, "data.img"), os.O_WRONLY, 0)
		if e != nil {
			return e
		}
		e = f.Truncate(m.DataBytes)
		if e == nil {
			e = f.Sync()
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
	}
	if e := saveJSON(filepath.Join(state, "release.json"), m); e != nil {
		return e
	}
	return os.Rename(filepath.Join(stage, "data.img"), path)
}

// Cold backups deliberately require the lifecycle lock: no guest may write while
// its disk is read. Restore only accepts an empty destination, retaining originals.
func backup(state, file string) error {
	if file == "" {
		return errors.New("--file is required; stop the cluster before backup")
	}
	if e := outsideState(state, file); e != nil {
		return e
	}
	id, e := age.GenerateX25519Identity()
	if e != nil {
		return e
	}
	// A recovery key is never silently overwritten or placed inside cluster state.
	keyPath := file + ".key"
	key, e := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = key.WriteString(id.String() + "\n")
	if e == nil {
		e = key.Sync()
	}
	e = errors.Join(e, key.Close())
	if e != nil {
		return e
	}
	if e = createBackup(state, file, id.Recipient()); e != nil {
		os.Remove(keyPath) // a key for a backup that does not exist is only clutter
		return e
	}
	fmt.Printf("Backup: %s\nRecovery key: %s\nMove the recovery key to a separate safe location.\n", file, keyPath)
	return nil
}

func outsideState(state, file string) error {
	abs, e := filepath.Abs(file)
	if e != nil {
		return e
	}
	rel, e := filepath.Rel(state, abs)
	if e != nil {
		return e
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("save backups outside cluster state")
	}
	return nil
}

// createBackup writes one encrypted copy of a stopped cluster's state to file, which must not
// exist. It holds the state lock, so it cannot run while the appliance is.
func createBackup(state, file string, recipient age.Recipient) error {
	unlock, e := lockState(state)
	if e != nil {
		return e
	}
	defer unlock()
	if e := checkUpdateStart(state); e != nil {
		return fmt.Errorf("update is unconfirmed; commit or cancel before a normal backup (cold recovery point remains retained): %w", e)
	}
	// A surviving gateway supervisor must finish reaping Caddy before its CA
	// store can be read, even if the original launcher crashed.
	if _, e = os.Stat(filepath.Join(state, "gateway-lock")); e == nil {
		unlockGateway, err := lockState(filepath.Join(state, "gateway-lock"))
		if err != nil {
			return err
		}
		defer unlockGateway()
	} else if !os.IsNotExist(e) {
		return e
	}
	names, memberErr := backupMembers(state)
	if memberErr != nil {
		return memberErr
	}
	out, e := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(file)
		}
	}()
	encrypted, e := age.Encrypt(out, recipient)
	if e != nil {
		return e
	}
	gz, e := gzip.NewWriterLevel(encrypted, gzip.BestSpeed) // the disk is mostly zeros, which compress at any level
	if e != nil {
		return e
	}
	tw := tar.NewWriter(gz)
	for _, name := range names {
		info, e := os.Lstat(filepath.Join(state, filepath.FromSlash(name)))
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || !validBackupMember(name, info.Size()) {
			return errors.New("invalid backup member")
		}
		f, e := os.Open(filepath.Join(state, name))
		if e != nil {
			return e
		}
		s, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		if name == "data.img" {
			if done, e := writeSparseDisk(tw, f, s.Size()); done {
				f.Close()
				if e != nil {
					return e
				}
				continue
			}
		}
		e = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: s.Size()})
		if e == nil {
			_, e = io.Copy(tw, f)
		}
		f.Close()
		if e != nil {
			return e
		}
	}
	if e = errors.Join(tw.Close(), gz.Close(), encrypted.Close(), out.Sync()); e != nil {
		return e
	}
	ok = true
	return nil
}
func restore(state, file, keyPath string) error {
	if file == "" || keyPath == "" {
		return errors.New("--file and --key are required")
	}
	if e := os.MkdirAll(state, 0700); e != nil {
		return e
	}
	unlock, e := lockState(state)
	if e != nil {
		return e
	}
	defer unlock()
	entries, e := os.ReadDir(state)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if entry.Name() != "clinic.lock" {
			return errors.New("restore requires an empty destination")
		}
	}
	for _, n := range []string{"data.img", "release.json"} {
		if _, e = os.Lstat(filepath.Join(state, n)); !os.IsNotExist(e) {
			return fmt.Errorf("restore requires an empty destination (%s)", n)
		}
	}
	k, e := os.ReadFile(keyPath)
	if e != nil {
		return e
	}
	id, e := age.ParseX25519Identity(strings.TrimSpace(string(k)))
	if e != nil {
		return e
	}
	in, e := os.Open(file)
	if e != nil {
		return e
	}
	defer in.Close()
	dec, e := age.Decrypt(in, id)
	if e != nil {
		return e
	}
	gz, e := gzip.NewReader(dec)
	if e != nil {
		return e
	}
	defer gz.Close()
	stage, e := os.MkdirTemp(state, "restore-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var sparse sparseMap
	haveSparse := false
	var metadataBytes int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if !validBackupMember(h.Name, h.Size) || seen[h.Name] || h.Typeflag != tar.TypeReg || len(seen) >= 4096 {
			return errors.New("invalid backup member")
		}
		if h.Name != "data.img" {
			metadataBytes += h.Size
			if metadataBytes > 64<<20 {
				return errors.New("backup metadata exceeds limit")
			}
		}
		if h.Name == sparseMapName {
			if seen["data.img"] {
				return errors.New("the sparse map must come before the data")
			}
			raw, e := io.ReadAll(io.LimitReader(tr, h.Size))
			if e != nil || json.Unmarshal(raw, &sparse) != nil {
				return errors.New("invalid sparse map")
			}
			haveSparse = true
			seen[h.Name] = true
			continue
		}
		seen[h.Name] = true
		if e := os.MkdirAll(filepath.Dir(filepath.Join(stage, filepath.FromSlash(h.Name))), 0700); e != nil {
			return e
		}
		f, e := os.OpenFile(filepath.Join(stage, h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		if h.Name == "data.img" && haveSparse {
			e = readSparseDisk(f, tr, sparse, h.Size)
		} else {
			_, e = io.Copy(f, tr)
		}
		if e == nil {
			e = f.Sync()
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
	}
	if _, e = io.Copy(io.Discard, gz); e != nil {
		return e
	} // Verify trailing AEAD/gzip data before publication.
	if !seen["data.img"] || !seen["release.json"] {
		return errors.New("incomplete backup")
	}
	var m manifest
	b, e := os.ReadFile(filepath.Join(stage, "release.json"))
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &m); e != nil {
		return e
	}
	if m.Arch != runtime.GOARCH {
		return errors.New("cold disk restore requires matching architecture; use a logical export to migrate architectures")
	}
	if seen["gateway.json"] {
		b, e := os.ReadFile(filepath.Join(stage, "gateway.json"))
		if e != nil {
			return e
		}
		var cfg gatewayConfig
		if e = decodeGatewayConfig(b, &cfg); e != nil {
			return e
		}
		if e = cfg.validate(); e != nil {
			return e
		}
	}
	// Publish identity first. Interrupted publication refuses to create an empty cluster.
	if e = atomicWrite(filepath.Join(state, "release.json"), b, 0600); e != nil {
		return e
	}
	for _, name := range []string{"gateway", "gateway.json"} {
		if _, e := os.Stat(filepath.Join(stage, name)); e == nil {
			if name == "gateway" {
				if e = privateGatewayDir(filepath.Join(stage, name)); e != nil {
					return e
				}
			}
			if e = os.Rename(filepath.Join(stage, name), filepath.Join(state, name)); e != nil {
				return e
			}
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	if e = os.Rename(filepath.Join(stage, "data.img"), filepath.Join(state, "data.img")); e != nil {
		return e
	}
	fmt.Println("Restored. Start with the matching release bundle and verify the cluster.")
	return nil
}
