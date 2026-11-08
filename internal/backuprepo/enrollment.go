package backuprepo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

const registryName = "recovery-backup-destinations.json"

// Destination is host-only configuration. Send Status, never Destination, to a
// Kubernetes controller. No private-key location is persisted in either file.
type Destination struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type EnrollmentOptions struct {
	Name      string
	Path      string
	Recipient string
	// KeyPath is an explicit new private-key filename outside state/repository.
	// To reuse a key, supply its public Recipient instead.
	KeyPath      string
	KeepLast     int
	MinFreeBytes uint64
}

func loadDestinations(root *os.Root) ([]Destination, error) {
	var ds []Destination
	if err := readJSON(root, registryName, &ds); err != nil {
		if os.IsNotExist(err) {
			return []Destination{}, nil
		}
		return nil, err
	}
	if len(ds) > 16 {
		return nil, errors.New("invalid recovery destination registry")
	}
	seen := map[string]bool{}
	for _, d := range ds {
		if !ValidID(d.ID) || seen[d.ID] || !filepath.IsAbs(d.Path) || filepath.Clean(d.Path) != d.Path {
			return nil, errors.New("invalid recovery destination record")
		}
		seen[d.ID] = true
	}
	return ds, nil
}

func saveDestinations(root *os.Root, ds []Destination) error {
	_, err := root.Lstat(registryName)
	return writeJSON(root, registryName, ds, os.IsNotExist(err))
}

// Destinations reads host enrollment without accessing any mounted repository.
func Destinations(state string) ([]Destination, error) {
	root, err := openRoot(state)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	unlock, err := acquireNamed(root, ".recovery-backup.lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	return loadDestinations(root)
}

func generateKey(state, repoPath, keyPath string) (string, error) {
	return generateKeyWithPublish(state, repoPath, keyPath, publishPrivate)
}

func publishPrivate(root *os.Root, from, to string) error {
	return publishWithCreate(root, from, to, root.Link, io.Copy, createPrivateFile)
}

func generateKeyWithPublish(state, repoPath, keyPath string, publishKey func(*os.Root, string, string) error) (string, error) {
	if !filepath.IsAbs(keyPath) || filepath.Clean(keyPath) != keyPath || Within(repoPath, keyPath) || Within(state, keyPath) {
		return "", errors.New("choose an absolute new key path outside repository and cluster state")
	}
	parent := filepath.Dir(keyPath)
	root, err := openRoot(parent)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if _, err := root.Lstat(filepath.Base(keyPath)); !os.IsNotExist(err) {
		return "", errors.New("recovery key already exists or is inaccessible; use its public recipient instead")
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	// A private staging directory gives key files owner-only inherited Windows
	// ACLs as well as Unix 0600. Publication preserves that security descriptor.
	dir, err := os.MkdirTemp(parent, ".recovery-key-")
	if err != nil {
		return "", err
	}
	removeStage := true
	defer func() {
		if removeStage {
			os.RemoveAll(dir)
		}
	}()
	if err := privateDirectory(dir); err != nil {
		return "", err
	}
	privateRoot, err := openRoot(dir)
	if err != nil {
		return "", err
	}
	defer privateRoot.Close()
	f, tmp, err := temporary(privateRoot)
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(identity.String() + "\n")
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return "", err
	}
	// Both names are anchored in the same parent; neither link nor copy may
	// overwrite. Retain the complete staged key on uncertain/partial publication.
	from := filepath.Join(filepath.Base(dir), tmp)
	if err := publishKey(root, from, filepath.Base(keyPath)); err != nil {
		removeStage = false
		return "", fmt.Errorf("key publication failed; complete key retained at %s: %w", filepath.Join(dir, tmp), err)
	}
	return identity.Recipient().String(), nil
}

// Enroll creates a new repository, or enrolls an existing matching repository
// by public recipient. It never creates the destination or overwrites a key.
// A generated key is retained even on later failure, avoiding accidental loss
// after uncertain publication; the operator can reuse its public recipient.
func Enroll(state string, o EnrollmentOptions) (Destination, error) {
	var out Destination
	if _, err := ExistingDirectory(state); err != nil {
		return out, err
	}
	if _, err := ExistingDirectory(o.Path); err != nil {
		return out, err
	}
	if Within(state, o.Path) || Within(o.Path, state) {
		return out, errors.New("repository and cluster state must be separate directories")
	}
	o.Name = strings.TrimSpace(o.Name)
	if o.Name == "" || len(o.Name) > 80 || strings.ContainsAny(o.Name, "\x00\r\n") {
		return out, errors.New("destination name must be 1..80 characters")
	}
	if (o.Recipient == "") == (o.KeyPath == "") {
		return out, errors.New("supply either a public --recipient or an explicit separate new --key path")
	}
	root, err := openRoot(state)
	if err != nil {
		return out, err
	}
	defer root.Close()
	unlock, err := acquireNamed(root, ".recovery-backup.lock")
	if err != nil {
		return out, err
	}
	defer unlock()
	ds, err := loadDestinations(root)
	if err != nil {
		return out, err
	}
	if len(ds) >= 16 {
		return out, errors.New("at most 16 recovery destinations may be enrolled")
	}
	for _, d := range ds {
		if d.Path == o.Path {
			return out, errors.New("repository is already enrolled")
		}
	}
	if o.KeyPath != "" {
		o.Recipient, err = generateKey(state, o.Path, o.KeyPath)
		if err != nil {
			return out, err
		}
	}
	if _, err := age.ParseX25519Recipient(o.Recipient); err != nil {
		return out, err
	}
	r, err := Open(o.Path)
	if os.IsNotExist(err) {
		r, err = Initialize(o.Path, Config{Recipient: o.Recipient, KeepLast: o.KeepLast, MinFreeBytes: o.MinFreeBytes})
	}
	if err != nil {
		return out, err
	}
	defer r.Close()
	if r.Config.Recipient != o.Recipient {
		return out, errors.New("repository recipient differs from enrollment recipient")
	}
	for _, d := range ds {
		if d.ID == r.Config.ID {
			return out, errors.New("repository identity is already enrolled")
		}
	}
	out = Destination{ID: r.Config.ID, Name: o.Name, Path: o.Path}
	return out, saveDestinations(root, append(ds, out))
}

// Forget removes enrollment only. It never removes backup files or keys.
func Forget(state, id string) error {
	if !ValidID(id) {
		return errors.New("invalid destination ID")
	}
	root, err := openRoot(state)
	if err != nil {
		return err
	}
	defer root.Close()
	unlock, err := acquireNamed(root, ".recovery-backup.lock")
	if err != nil {
		return err
	}
	defer unlock()
	ds, err := loadDestinations(root)
	if err != nil {
		return err
	}
	for i, d := range ds {
		if d.ID == id {
			return saveDestinations(root, append(ds[:i], ds[i+1:]...))
		}
	}
	return os.ErrNotExist
}

// Resolve requires both a host enrollment and the matching on-disk identity.
// An absent removable drive or an empty replacement mount point is an error.
func Resolve(state, id string) (*Repository, error) {
	if !ValidID(id) {
		return nil, errors.New("invalid destination ID")
	}
	ds, err := Destinations(state)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if d.ID != id {
			continue
		}
		r, err := Open(d.Path)
		if err != nil {
			return nil, err
		}
		if r.Config.ID != id {
			r.Close()
			return nil, errors.New("a different repository is mounted at the enrolled location")
		}
		return r, nil
	}
	return nil, fmt.Errorf("unknown recovery destination %q", id)
}

// Facts intentionally reports availability without filesystem error strings,
// which can contain private host paths. Detailed errors belong on the host UI.
func Facts(state string) ([]Status, error) {
	ds, err := Destinations(state)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(ds))
	for _, d := range ds {
		s := Status{ID: d.ID, Name: d.Name}
		r, err := Open(d.Path)
		if err == nil {
			if r.Config.ID == d.ID {
				s.MinFreeBytes = r.Config.MinFreeBytes
				entries, e := r.List()
				if errors.Is(e, ErrLocked) {
					s.Available = true
					s.Busy = true
				}
				if e == nil {
					s.Available = true
					s.BackupCount = len(entries)
					for _, entry := range entries {
						created := entry.CreatedAt
						if s.LastCompleted == nil || created.After(*s.LastCompleted) {
							s.LastCompleted = &created
						}
						if entry.VerifiedAt != nil && (s.LastVerified == nil || entry.VerifiedAt.After(*s.LastVerified)) {
							s.LastVerified = entry.VerifiedAt
						}
					}
				}
				if s.Available {
					if usage, e := diskspace.Of(d.Path); e == nil {
						s.FreeBytes = usage.Free
						s.TotalBytes = usage.Total
						s.ReserveBytes = EffectiveReserve(usage.Total, r.Config.MinFreeBytes)
						s.Ready = !s.Busy && spaceReady(usage, 1<<20, r.Config.MinFreeBytes) && len(entries) <= r.Config.KeepLast && retainedCount(entries) < r.Config.KeepLast
					} else {
						s.Available = false
					}
				}
			}
			r.Close()
		}
		out = append(out, s)
	}
	return out, nil
}
