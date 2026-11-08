package recoveryapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,127}$`)

// Store must provide durable replace and complete listing. A missing/corrupt
// catalog is not permission to delete snapshots. One controller owns a store.
type Store interface {
	Get(string) (Record, error)
	Put(Record) error
	List() ([]Record, error)
}

type FileStore struct{ Dir string }

func (s FileStore) path(uid string) (string, error) {
	if s.Dir == "" || !safeID.MatchString(uid) {
		return "", errors.New("invalid recovery journal directory or UID")
	}
	return filepath.Join(s.Dir, uid+".json"), nil
}

func (s FileStore) Get(uid string) (Record, error) {
	p, err := s.path(uid)
	if err != nil {
		return Record{}, err
	}
	lst, err := os.Lstat(p)
	if err != nil {
		return Record{}, err
	}
	if !lst.Mode().IsRegular() {
		return Record{}, errors.New("journal must be a regular file")
	}
	f, err := os.Open(p)
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Record{}, err
	}
	if !st.Mode().IsRegular() || st.Size() > 8<<20 {
		return Record{}, errors.New("invalid recovery journal file")
	}
	var r Record
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return r, errors.New("trailing recovery journal data")
	}
	if r.UID != uid {
		return r, errors.New("recovery journal UID mismatch")
	}
	return r, nil
}

func (s FileStore) Put(r Record) error {
	p, err := s.path(r.UID)
	if err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > 8<<20 {
		return errors.New("recovery journal exceeds 8 MiB")
	}
	return atomicfile.WritePrivate(p, b)
}

func (s FileStore) List() ([]Record, error) {
	if s.Dir == "" {
		return nil, errors.New("recovery journal directory is required")
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, errors.New("recovery catalog exceeds 10000 entries; administrator review required")
	}
	out := []Record{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("journal symlink: %s", e.Name())
		}
		r, err := s.Get(e.Name()[:len(e.Name())-5])
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// RetentionCandidates never returns the newest successful set, failed/partial
// sets, a pending export or any set referenced by a restore. Keep counts are
// independent per scope and namespace identity, including after policy edits.
func RetentionCandidates(records []Record, policyUID string, keep int) []string {
	if keep < 1 {
		return nil
	}
	var successful []Record
	for _, r := range records {
		if r.PolicyUID == policyUID && r.Phase == "Succeeded" && (r.Kind == "RecoveryPoint" || r.Kind == "ClusterRecoveryPoint") && r.CompletedAt != nil {
			successful = append(successful, r)
		}
	}
	sort.Slice(successful, func(i, j int) bool {
		if successful[i].CompletedAt.Equal(*successful[j].CompletedAt) {
			return successful[i].UID < successful[j].UID
		}
		return successful[i].CompletedAt.After(*successful[j].CompletedAt)
	})
	var out []string
	counts := map[string]int{}
	for _, r := range successful {
		group := r.Kind + "/" + r.NamespaceUID + "/" + r.Scope
		counts[group]++
		if counts[group] > keep && !r.ExportPending && len(r.ProtectedBy) == 0 {
			out = append(out, r.UID)
		}
	}
	return out
}
