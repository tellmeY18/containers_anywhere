package backuprepo

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

// StagedExport contains a completely authenticated plaintext export. Close
// removes its private staging directory. Never forward a partially decrypted
// reader to the guest: age authenticates in chunks, not all at once.
type StagedExport struct {
	Entry Entry
	File  *os.File
	Path  string
	dir   string
}

func (s *StagedExport) Close() error {
	return errors.Join(s.File.Close(), os.RemoveAll(s.dir))
}

func (r *Repository) identities(keyPath string) ([]age.Identity, error) {
	if !filepath.IsAbs(keyPath) || filepath.Clean(keyPath) != keyPath {
		return nil, errors.New("an absolute, clean, separate key path is required")
	}
	if Within(r.Path, keyPath) {
		return nil, errors.New("recovery key must not be stored in the repository")
	}
	parent, err := openRoot(filepath.Dir(keyPath))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err := regular(parent, filepath.Base(keyPath)); err != nil {
		return nil, err
	}
	f, err := parent.Open(filepath.Base(keyPath))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, err
	}
	if len(b) > 65536 {
		return nil, errors.New("recovery key file is too large")
	}
	ids, err := age.ParseIdentities(strings.NewReader(string(b)))
	if err == nil && len(ids) == 0 {
		err = errors.New("recovery key file has no identities")
	}
	return ids, err
}

func (r *Repository) decrypt(ctx context.Context, e Entry, identities []age.Identity, dst io.Writer) error {
	name := e.ID + ".age"
	if err := regular(r.root, name); err != nil {
		return err
	}
	f, err := r.root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil {
		return err
	}
	if s.Size() != e.CipherBytes {
		return errors.New("encrypted backup size mismatch")
	}
	h := sha256.New()
	plain, err := age.Decrypt(io.TeeReader(f, h), identities...)
	if err != nil {
		return err
	}
	prefix := make([]byte, len(magic))
	if _, err := io.ReadFull(plain, prefix); err != nil {
		return err
	}
	if string(prefix) != magic {
		return errors.New("unsupported encrypted recovery format")
	}
	var n uint32
	if err := binary.Read(plain, binary.BigEndian, &n); err != nil {
		return err
	}
	if n == 0 || n > 65536 {
		return errors.New("invalid authenticated metadata length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(plain, b); err != nil {
		return err
	}
	var meta Metadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return err
	}
	if meta.Version != e.Version || meta.ID != e.ID || meta.PointID != e.PointID || meta.Scope != e.Scope || meta.Mode != e.Mode || meta.Transport != e.Transport || !meta.CreatedAt.Equal(e.CreatedAt) {
		return errors.New("catalog does not match authenticated backup metadata")
	}
	// Consume through authenticated EOF, including age's final chunk. Limit
	// plaintext to the catalog size so a forged catalog cannot exhaust staging.
	w := &guardedWriter{ctx: ctx, w: dst, untilCheck: 1 << 62}
	written, err := io.Copy(w, io.LimitReader(plain, e.PlainBytes+1))
	if err != nil {
		return fmt.Errorf("authenticate complete backup: %w", err)
	}
	if written != e.PlainBytes {
		return errors.New("plaintext backup size mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return errors.New("encrypted backup checksum mismatch")
	}
	return nil
}

func findEntry(c catalog, id string) (Entry, int, error) {
	if !ValidID(id) {
		return Entry{}, 0, errors.New("invalid backup ID")
	}
	for i, e := range c.Entries {
		if e.ID == id {
			return e, i, nil
		}
	}
	return Entry{}, 0, os.ErrNotExist
}

// Verify authenticates every byte without a running guest or staging disk.
// Success records integrity verification, not application restore acceptance.
func (r *Repository) Verify(ctx context.Context, id, keyPath string) (Entry, error) {
	unlock, err := acquire(r.root)
	if err != nil {
		return Entry{}, err
	}
	defer unlock()
	c, err := r.load()
	if err != nil {
		return Entry{}, err
	}
	e, i, err := findEntry(c, id)
	if err != nil {
		return Entry{}, err
	}
	ids, err := r.identities(keyPath)
	if err != nil {
		return Entry{}, err
	}
	if err := r.decrypt(ctx, e, ids, io.Discard); err != nil {
		return Entry{}, err
	}
	now := time.Now().UTC()
	e.VerifiedAt = &now
	c.Entries[i] = e
	if err := writeJSON(r.root, catalogName, c, false); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Stage authenticates the entire backup into a private, fsynced staging file
// before making it available to its caller. The guest remains responsible for
// validating its archive members, compatibility, and empty restore target.
func (r *Repository) Stage(ctx context.Context, id, keyPath, stagingDir string) (*StagedExport, error) {
	if _, err := ExistingDirectory(stagingDir); err != nil {
		return nil, err
	}
	if Within(r.Path, stagingDir) {
		return nil, errors.New("use a staging directory separate from the repository")
	}
	unlock, err := acquire(r.root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	c, err := r.load()
	if err != nil {
		return nil, err
	}
	e, i, err := findEntry(c, id)
	if err != nil {
		return nil, err
	}
	ids, err := r.identities(keyPath)
	if err != nil {
		return nil, err
	}
	if err := checkSpace(stagingDir, uint64(e.PlainBytes), r.Config.MinFreeBytes, r.Config.DiskSpace); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(stagingDir, ".recovery-import-")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	root, err := openRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, tmp, err := temporary(root)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	w := &guardedWriter{ctx: ctx, w: f, path: dir, floor: r.Config.MinFreeBytes, root: root, space: r.Config.DiskSpace}
	if err := r.decrypt(ctx, e, ids, w); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	copyData := func(dst io.Writer, src io.Reader) (int64, error) {
		if err := checkSpace(dir, uint64(e.PlainBytes), r.Config.MinFreeBytes, r.Config.DiskSpace); err != nil {
			return 0, err
		}
		return io.Copy(&guardedWriter{ctx: ctx, w: dst, path: dir, floor: r.Config.MinFreeBytes, root: root, space: r.Config.DiskSpace}, src)
	}
	if err := publishWith(root, tmp, "verified.export", root.Link, copyData); err != nil {
		return nil, err
	}
	if err := root.Remove(tmp); err != nil {
		return nil, err
	}
	ready, err := root.Open("verified.export")
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	e.VerifiedAt = &now
	c.Entries[i] = e
	if err := writeJSON(r.root, catalogName, c, false); err != nil {
		ready.Close()
		return nil, err
	}
	ok = true
	return &StagedExport{Entry: e, File: ready, Path: filepath.Join(dir, "verified.export"), dir: dir}, nil
}
