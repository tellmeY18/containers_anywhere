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
	unlock, e := lockState(state)
	if e != nil {
		return e
	}
	defer unlock()
	if _, e = os.Lstat(filepath.Join(state, "update.json")); !os.IsNotExist(e) {
		return errors.New("update is unconfirmed; recover the pre-update snapshot before backing up")
	}
	id, e := age.GenerateX25519Identity()
	if e != nil {
		return e
	}
	// A recovery key is never silently overwritten or placed inside cluster state.
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
	encrypted, e := age.Encrypt(out, id.Recipient())
	if e != nil {
		return e
	}
	gz := gzip.NewWriter(encrypted)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"release.json", "data.img"} {
		f, e := os.Open(filepath.Join(state, name))
		if e != nil {
			return e
		}
		s, e := f.Stat()
		if e != nil {
			f.Close()
			return e
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
	fmt.Printf("Backup: %s\nRecovery key: %s\nMove the recovery key to a separate safe location.\n", file, keyPath)
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
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if (h.Name != "data.img" && h.Name != "release.json") || seen[h.Name] || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > 64<<30 {
			return errors.New("invalid backup member")
		}
		seen[h.Name] = true
		f, e := os.OpenFile(filepath.Join(stage, h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(f, tr)
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
	if len(seen) != 2 {
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
	// Publish identity first. Interrupted publication refuses to create an empty cluster.
	if e = atomicWrite(filepath.Join(state, "release.json"), b, 0600); e != nil {
		return e
	}
	if e = os.Rename(filepath.Join(stage, "data.img"), filepath.Join(state, "data.img")); e != nil {
		return e
	}
	fmt.Println("Restored. Start with the matching release bundle and verify the cluster.")
	return nil
}
