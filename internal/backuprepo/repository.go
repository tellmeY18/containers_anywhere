// Package backuprepo stores independent, age-encrypted full recovery exports.
// It has no dependency on a running VM, Kubernetes, or a cluster data disk.
package backuprepo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

const (
	Version                 = 1
	TransportFull           = "guest-recovery-full-v1"
	DefaultFreeFloor uint64 = 20 << 30
	markerName              = "repository.json"
	catalogName             = "catalog.json"
	magic                   = "CA-RECOVERY-BACKUP-1\n"
	maxMetadata             = 1 << 20
)

var (
	ErrLocked = errors.New("backup repository is busy")
	validID   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,95}$`)
)

// ValidID accepts only single, portable path components, including on Windows.
func ValidID(id string) bool {
	if !validID.MatchString(id) {
		return false
	}
	upper := strings.ToUpper(id)
	switch upper {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9' {
		return false
	}
	return true
}

type Config struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	Recipient    string `json:"recipient"`
	KeepLast     int    `json:"keepLast"`
	MinFreeBytes uint64 `json:"minFreeBytes"`
	// DiskSpace is an optional runtime dependency for trusted host callers and
	// deterministic tests. It is never persisted or accepted from JSON config.
	DiskSpace func(string) (diskspace.Usage, error) `json:"-"`
}

// Metadata is authenticated inside every encrypted file. The payload is the
// guest's complete export (including its compatibility manifest), not a delta.
type Metadata struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	PointID   string    `json:"pointID"`
	Transport string    `json:"transport"`
	Scope     string    `json:"scope"`
	Mode      string    `json:"mode"`
	CreatedAt time.Time `json:"createdAt"`
}

type Entry struct {
	Metadata
	PlainBytes  int64      `json:"plainBytes"`
	CipherBytes int64      `json:"cipherBytes"`
	SHA256      string     `json:"sha256"`
	Protected   bool       `json:"protected"`
	VerifiedAt  *time.Time `json:"verifiedAt,omitempty"`
}

type catalog struct {
	Version          int      `json:"version"`
	Entries          []Entry  `json:"entries"`
	PendingDeletions []string `json:"pendingDeletions,omitempty"`
}

type Repository struct {
	Path   string
	Config Config
	root   *os.Root
	// Per-handle publication operations allow fault-injection without global
	// mutable hooks or depending on a particular CI runner filesystem.
	linkFile   func(string, string) error
	copyFile   func(io.Writer, io.Reader) (int64, error)
	removeFile func(string) error
}

type ExportOptions struct {
	ID        string
	PointID   string
	Scope     string
	Protected bool
	// ExpectedBytes is the complete guest payload length; -1 means unknown.
	ExpectedBytes int64
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ExistingDirectory rejects relative paths, traversal spelling, and symlinks in
// any component. It never creates a missing mount point or replacement folder.
func ExistingDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("an absolute, clean directory path is required")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("directory is unavailable (connect its enrolled drive): %w", err)
	}
	if real != path {
		return "", errors.New("symlink directory components are not allowed; use the real path")
	}
	s, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !s.IsDir() {
		return "", errors.New("expected an existing directory")
	}
	return path, nil
}

func Within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func openRoot(path string) (*os.Root, error) {
	if _, err := ExistingDirectory(path); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	if err := sameDirectory(root, path); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func sameDirectory(root *os.Root, path string) error {
	if _, err := ExistingDirectory(path); err != nil {
		return err
	}
	anchored, err := root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(anchored, current) {
		return errors.New("repository directory was replaced or its drive disconnected")
	}
	return nil
}

func regular(root *os.Root, name string) error {
	s, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !s.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not a link", name)
	}
	return nil
}

func readJSON(root *os.Root, name string, out any) error {
	if err := regular(root, name); err != nil {
		return err
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxMetadata+1))
	if err != nil {
		return err
	}
	if len(b) > maxMetadata {
		return errors.New("repository metadata is too large")
	}
	return json.Unmarshal(b, out)
}

func temporary(root *os.Root) (*os.File, string, error) {
	id, err := randomID()
	if err != nil {
		return nil, "", err
	}
	name := ".pending-" + id
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	return f, name, err
}

// publish prefers a hard link. Filesystems without links use exclusive-copy
// publication: file appearance is not atomic, but the catalog remains the only
// completed-publication boundary. An interrupted copy is an unlisted orphan.
func publish(root *os.Root, from, to string) error {
	return publishWith(root, from, to, root.Link, io.Copy)
}

func publishWith(root *os.Root, from, to string, link func(string, string) error, copyData func(io.Writer, io.Reader) (int64, error)) error {
	return publishWithCreate(root, from, to, link, copyData, func(root *os.Root, name string) (*os.File, error) {
		return root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	})
}

func publishWithCreate(root *os.Root, from, to string, link func(string, string) error, copyData func(io.Writer, io.Reader) (int64, error), create func(*os.Root, string) (*os.File, error)) error {
	if err := link(from, to); err != nil {
		if !errors.Is(err, errors.ErrUnsupported) && !linkUnsupported(root, err) {
			return err
		}
		return publishCopyExclusive(root, from, to, copyData, create)
	}
	return syncRoot(root)
}

// Never remove the destination, even after an error: another file must never be
// overwritten or mistaken for our partial copy. The caller retains the source
// as appropriate (notably a complete recovery key after publication failure).
func publishCopyExclusive(root *os.Root, from, to string, copyData func(io.Writer, io.Reader) (int64, error), create func(*os.Root, string) (*os.File, error)) error {
	if err := regular(root, from); err != nil {
		return err
	}
	in, err := root.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := create(root, to)
	if err != nil {
		return err
	}
	n, err := copyData(out, in)
	if err == nil && n != info.Size() {
		err = errors.New("publication copy length mismatch")
	}
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		return err
	}
	return syncRoot(root)
}

func writeJSON(root *os.Root, name string, value any, exclusive bool) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, tmp, err := temporary(root)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if exclusive {
		return publish(root, tmp, name)
	}
	if err := regular(root, name); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	return syncRoot(root)
}

func acquire(root *os.Root) (func(), error) {
	return acquireNamed(root, ".repository.lock")
}

func acquireNamed(root *os.Root, name string) (func(), error) {
	if err := regular(root, name); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return lockFile(f)
}

// Initialize requires an existing empty directory (apart from our lock file).
// A public X25519 recipient suffices; no private key is ever stored here.
func Initialize(path string, config Config) (*Repository, error) {
	root, err := openRoot(path)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			root.Close()
		}
	}()
	unlock, err := acquire(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if name != ".repository.lock" {
			return nil, errors.New("repository enrollment requires an empty directory")
		}
	}
	if _, err := age.ParseX25519Recipient(config.Recipient); err != nil {
		return nil, err
	}
	if config.ID == "" {
		config.ID, err = randomID()
		if err != nil {
			return nil, err
		}
	}
	config.Version = Version
	if config.KeepLast == 0 {
		config.KeepLast = 8
	}
	if config.MinFreeBytes < DefaultFreeFloor {
		config.MinFreeBytes = DefaultFreeFloor
	}
	if !ValidID(config.ID) || config.KeepLast < 1 || config.KeepLast > 1024 {
		return nil, errors.New("invalid repository ID or retention (1..1024)")
	}
	if err := writeJSON(root, catalogName, catalog{Version: Version, Entries: []Entry{}}, true); err != nil {
		return nil, err
	}
	if err := writeJSON(root, markerName, config, true); err != nil {
		return nil, err
	}
	ok = true
	return &Repository{Path: path, Config: config, root: root}, nil
}

// Open needs only the repository path; it does not read host enrollment/state.
func Open(path string) (*Repository, error) {
	root, err := openRoot(path)
	if err != nil {
		return nil, err
	}
	var c Config
	err = readJSON(root, markerName, &c)
	if err == nil && (c.Version != Version || !ValidID(c.ID) || c.KeepLast < 1 || c.KeepLast > 1024 || c.MinFreeBytes < DefaultFreeFloor) {
		err = errors.New("unsupported or invalid repository configuration")
	}
	if err == nil {
		_, err = age.ParseX25519Recipient(c.Recipient)
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	r := &Repository{Path: path, Config: c, root: root}
	unlock, err := acquire(root)
	if errors.Is(err, ErrLocked) {
		return r, nil
	}
	if err == nil {
		var cat catalog
		cat, err = r.load()
		if err == nil {
			err = r.resumeDeletions(&cat)
		}
		unlock()
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	return r, nil
}

func (r *Repository) Close() error { return r.root.Close() }

func (r *Repository) load() (catalog, error) {
	var c catalog
	if err := readJSON(r.root, catalogName, &c); err != nil {
		return c, err
	}
	if c.Version != Version || len(c.Entries) > 1025 || len(c.PendingDeletions) > 1024 {
		return c, errors.New("invalid catalog version or size")
	}
	seen := map[string]bool{}
	for _, e := range c.Entries {
		if e.Version != Version || !ValidID(e.ID) || !ValidID(e.PointID) || e.Transport != TransportFull || e.Mode != "Full" || !ValidID(e.Scope) || e.PlainBytes < 0 || e.PlainBytes >= 1<<60 || e.CipherBytes <= 0 || len(e.SHA256) != 64 || seen[e.ID] {
			return c, errors.New("invalid catalog entry")
		}
		seen[e.ID] = true
	}
	for _, id := range c.PendingDeletions {
		if !ValidID(id) || seen[id] {
			return c, errors.New("invalid pending deletion journal")
		}
		seen[id] = true
	}
	return c, nil
}

func (r *Repository) List() ([]Entry, error) {
	unlock, err := acquire(r.root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	c, err := r.load()
	return c.Entries, err
}

func checkSpace(path string, required, floor uint64, usage func(string) (diskspace.Usage, error)) error {
	if _, err := ExistingDirectory(path); err != nil {
		return err
	}
	if usage == nil {
		usage = diskspace.Of
	}
	u, err := usage(path)
	if err != nil {
		return err
	}
	if !spaceReady(u, required, floor) {
		return errors.New("insufficient free space above the recovery reserve")
	}
	return nil
}

// EffectiveReserve matches the host-growth floor: preserve at least 20 GiB,
// ten percent of this filesystem, or the configured larger reserve.
func EffectiveReserve(total, configured uint64) uint64 {
	return max(DefaultFreeFloor, total/10, configured)
}

func spaceReady(u diskspace.Usage, required, configured uint64) bool {
	floor := EffectiveReserve(u.Total, configured)
	return u.Free >= floor && required <= u.Free-floor
}

type guardedWriter struct {
	ctx        context.Context
	w          io.Writer
	path       string
	floor      uint64
	untilCheck int64
	root       *os.Root
	space      func(string) (diskspace.Usage, error)
}

func (w *guardedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.untilCheck <= 0 {
		if w.root != nil {
			if err := sameDirectory(w.root, w.path); err != nil {
				return 0, err
			}
		}
		if err := checkSpace(w.path, uint64(len(p))+(1<<20), w.floor, w.space); err != nil {
			return 0, err
		}
		w.untilCheck = 1 << 20
	}
	n, err := w.w.Write(p)
	w.untilCheck -= int64(n)
	return n, err
}

// Export commits only after EOF, age.Close and fsync. Failed or interrupted
// exports cannot prune old backups. Existing IDs and orphan files are refused.
func (r *Repository) Export(ctx context.Context, src io.Reader, o ExportOptions) (Entry, error) {
	var e Entry
	if o.ID == "" {
		var err error
		o.ID, err = randomID()
		if err != nil {
			return e, err
		}
	}
	if o.Scope == "" {
		o.Scope = "system"
	}
	if !ValidID(o.ID) || !ValidID(o.PointID) || !ValidID(o.Scope) || o.ExpectedBytes < -1 {
		return e, errors.New("invalid export options")
	}
	unlock, err := acquire(r.root)
	if err != nil {
		return e, err
	}
	defer unlock()
	c, err := r.load()
	if err != nil {
		return e, err
	}
	if err := r.resumeDeletions(&c); err != nil {
		return e, err
	}
	if len(c.Entries) > r.Config.KeepLast {
		return e, errors.New("interrupted retention requires Prune before another export")
	}
	for _, old := range c.Entries {
		if old.ID == o.ID {
			return e, os.ErrExist
		}
	}
	if _, err := r.root.Lstat(o.ID + ".age"); !os.IsNotExist(err) {
		if err == nil {
			err = os.ErrExist
		}
		return e, err
	}
	if retainedCount(c.Entries) >= r.Config.KeepLast {
		return e, errors.New("retention is full of protected/last-verified backups")
	}
	required := uint64(0)
	if o.ExpectedBytes >= 1<<60 {
		return e, errors.New("export is too large")
	}
	if o.ExpectedBytes >= 0 {
		required = uint64(o.ExpectedBytes)
		required += required/1024 + (1 << 20)
	}
	if err := checkSpace(r.Path, required, r.Config.MinFreeBytes, r.Config.DiskSpace); err != nil {
		return e, err
	}
	if err := sameDirectory(r.root, r.Path); err != nil {
		return e, err
	}
	e = Entry{Metadata: Metadata{Version: Version, ID: o.ID, PointID: o.PointID, Scope: o.Scope, Mode: "Full", Transport: TransportFull, CreatedAt: time.Now().UTC()}, Protected: o.Protected}
	f, tmp, err := temporary(r.root)
	if err != nil {
		return Entry{}, err
	}
	defer func() { f.Close(); r.root.Remove(tmp) }()
	recipient, err := age.ParseX25519Recipient(r.Config.Recipient)
	if err != nil {
		return Entry{}, err
	}
	h := sha256.New()
	w := &guardedWriter{ctx: ctx, w: io.MultiWriter(f, h), path: r.Path, floor: r.Config.MinFreeBytes, root: r.root, space: r.Config.DiskSpace}
	encrypted, err := age.Encrypt(w, recipient)
	if err != nil {
		return Entry{}, err
	}
	header, _ := json.Marshal(e.Metadata)
	if _, err = io.WriteString(encrypted, magic); err == nil {
		err = binary.Write(encrypted, binary.BigEndian, uint32(len(header)))
	}
	if err == nil {
		_, err = encrypted.Write(header)
	}
	if err == nil {
		limit := int64(1 << 60)
		if o.ExpectedBytes >= 0 {
			limit = o.ExpectedBytes + 1
		}
		e.PlainBytes, err = io.Copy(encrypted, io.LimitReader(src, limit))
		if err == nil && e.PlainBytes >= 1<<60 {
			err = errors.New("export is too large")
		}
	}
	if err == nil && o.ExpectedBytes >= 0 && e.PlainBytes != o.ExpectedBytes {
		err = errors.New("guest export length mismatch")
	}
	if err == nil {
		err = encrypted.Close()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		var s os.FileInfo
		s, err = f.Stat()
		if err == nil {
			e.CipherBytes = s.Size()
		}
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return Entry{}, err
	}
	e.SHA256 = hex.EncodeToString(h.Sum(nil))
	if err := sameDirectory(r.root, r.Path); err != nil {
		return Entry{}, err
	}
	copyData := func(dst io.Writer, src io.Reader) (int64, error) {
		if err := checkSpace(r.Path, uint64(e.CipherBytes), r.Config.MinFreeBytes, r.Config.DiskSpace); err != nil {
			return 0, err
		}
		copyFile := r.copyFile
		if copyFile == nil {
			copyFile = io.Copy
		}
		return copyFile(&guardedWriter{ctx: ctx, w: dst, path: r.Path, floor: r.Config.MinFreeBytes, root: r.root, space: r.Config.DiskSpace}, src)
	}
	link := r.linkFile
	if link == nil {
		link = r.root.Link
	}
	if err := publishWith(r.root, tmp, e.ID+".age", link, copyData); err != nil {
		return Entry{}, err
	}
	// The catalog is the publication boundary. An orphan .age after a crash is
	// deliberately retained and never overwritten or advertised as complete.
	c.Entries = append(c.Entries, e)
	if err := writeJSON(r.root, catalogName, c, false); err != nil {
		return Entry{}, err
	}
	if err := r.prune(c); err != nil {
		return e, fmt.Errorf("backup committed; retention incomplete: %w", err)
	}
	return e, nil
}

func latestVerified(entries []Entry) string {
	var newest time.Time
	id := ""
	for _, e := range entries {
		if e.VerifiedAt != nil && (id == "" || e.VerifiedAt.After(newest)) {
			id, newest = e.ID, *e.VerifiedAt
		}
	}
	return id
}

func retainedCount(entries []Entry) int {
	keep := latestVerified(entries)
	n := 0
	for _, e := range entries {
		if e.Protected || e.ID == keep {
			n++
		}
	}
	return n
}

func (r *Repository) prune(c catalog) error {
	if err := r.resumeDeletions(&c); err != nil {
		return err
	}
	if len(c.Entries) <= r.Config.KeepLast {
		return nil
	}
	keep := latestVerified(c.Entries)
	// Preserve append order: wall-clock rollback must not prune the new export.
	newest := c.Entries[len(c.Entries)-1].ID
	for i := 0; len(c.Entries) > r.Config.KeepLast && i < len(c.Entries); {
		e := c.Entries[i]
		if e.Protected || e.ID == keep || e.ID == newest {
			i++
			continue
		}
		c.PendingDeletions = append(c.PendingDeletions, e.ID)
		c.Entries = append(c.Entries[:i], c.Entries[i+1:]...)
	}
	if len(c.PendingDeletions) == 0 {
		return nil
	}
	// Remove advertised entries and persist their exact deletion IDs in the
	// same catalog commit. Unrelated export orphans never enter this journal.
	if err := writeJSON(r.root, catalogName, c, false); err != nil {
		return err
	}
	return r.resumeDeletions(&c)
}

// Requires the repository lock. Missing journaled files mean a prior attempt
// deleted them before its catalog acknowledgement; every other error retains
// the journal for a later Open, Export or Prune retry. No directory scanning.
func (r *Repository) resumeDeletions(c *catalog) error {
	if len(c.PendingDeletions) == 0 {
		return nil
	}
	remove := r.removeFile
	if remove == nil {
		remove = r.root.Remove
	}
	for _, id := range c.PendingDeletions {
		name := id + ".age"
		if err := regular(r.root, name); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := syncRoot(r.root); err != nil {
		return err
	}
	updated := *c
	updated.PendingDeletions = nil
	if err := writeJSON(r.root, catalogName, updated, false); err != nil {
		return err
	}
	*c = updated
	return nil
}

func (r *Repository) Protect(id string, protected bool) error {
	if !ValidID(id) {
		return errors.New("invalid backup ID")
	}
	unlock, err := acquire(r.root)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := r.load()
	if err != nil {
		return err
	}
	for i := range c.Entries {
		if c.Entries[i].ID == id {
			c.Entries[i].Protected = protected
			return writeJSON(r.root, catalogName, c, false)
		}
	}
	return os.ErrNotExist
}

// Prune retries retention after a successfully committed export was interrupted
// before retention finished. It never removes protected or last-verified points.
func (r *Repository) Prune() error {
	unlock, err := acquire(r.root)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := r.load()
	if err != nil {
		return err
	}
	return r.prune(c)
}

// Status is safe to send to Kubernetes: no host path or recipient/key material.
type Status struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Available     bool       `json:"available"`
	Busy          bool       `json:"busy,omitempty"`
	Ready         bool       `json:"ready"`
	FreeBytes     uint64     `json:"freeBytes"`
	TotalBytes    uint64     `json:"totalBytes"`
	MinFreeBytes  uint64     `json:"minFreeBytes"`
	ReserveBytes  uint64     `json:"reserveBytes"`
	BackupCount   int        `json:"backupCount"`
	LastCompleted *time.Time `json:"lastCompleted,omitempty"`
	LastVerified  *time.Time `json:"lastVerified,omitempty"`
}
