package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/storage"
)

const (
	capacityQuantum   uint64 = 1 << 20
	metadataHeadroom  uint64 = 64 << 20
	maxStagingEntries        = 64
)

type Capacity struct {
	SizeBytes    uint64 `json:"sizeBytes"`
	FreeBytes    uint64 `json:"freeBytes"`
	ReserveBytes uint64 `json:"reserveBytes"`
}

// Capacity uses available (not root-reserved) bytes on the persistent snapshot
// filesystem. A 5% floor leaves Btrfs metadata/COW room; 1 GiB is the minimum on
// small disks. It is a preflight/continuous guard, not an allocation guarantee.
func (e *Engine) Capacity() (Capacity, error) {
	u, err := e.cfg.DiskSpace(e.cfg.Root)
	if err != nil {
		return Capacity{}, fmt.Errorf("guest recovery capacity: %w", err)
	}
	if u.Total == 0 || u.Free > u.Total {
		return Capacity{}, errors.New("invalid guest disk capacity report")
	}
	reserve := u.Total / 20
	if u.Total%20 != 0 {
		reserve++
	}
	if reserve < 1<<30 {
		reserve = 1 << 30
	}
	return Capacity{SizeBytes: u.Total, FreeBytes: u.Free, ReserveBytes: reserve}, nil
}

func (e *Engine) checkCapacity(additional uint64) error {
	c, err := e.Capacity()
	if err != nil {
		return err
	}
	if c.FreeBytes < c.ReserveBytes || additional > c.FreeBytes-c.ReserveBytes {
		return fmt.Errorf("%w: free=%d reserve=%d additional=%d bytes; inspect retained recovery staging before retrying", ErrCapacity, c.FreeBytes, c.ReserveBytes, additional)
	}
	return nil
}

// A reader/writer gets at most one MiB of unchecked progress. The extra MiB is
// checked before progress starts, so buffered/large writes cannot skip the floor.
type capacityWriter struct {
	e      *Engine
	w      io.Writer
	credit uint64
}

func (w *capacityWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if w.credit == 0 {
			if err := w.e.checkCapacity(capacityQuantum); err != nil {
				return written, err
			}
			w.credit = capacityQuantum
		}
		nwant := len(p)
		if uint64(nwant) > w.credit {
			nwant = int(w.credit)
		}
		n, err := w.w.Write(p[:nwant])
		written += n
		w.credit -= uint64(n)
		p = p[n:]
		if err != nil {
			return written, err
		}
		if n != nwant {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// capacityReadSeeker stays seekable so the Btrfs backend can validate and then
// receive the authenticated spool directly, without making another disk copy.
type capacityReadSeeker struct {
	e      *Engine
	r      io.ReadSeeker
	credit uint64
}

func (r *capacityReadSeeker) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.credit == 0 {
		if err := r.e.checkCapacity(capacityQuantum); err != nil {
			return 0, err
		}
		r.credit = capacityQuantum
	}
	if uint64(len(p)) > r.credit {
		p = p[:int(r.credit)]
	}
	n, err := r.r.Read(p)
	r.credit -= uint64(n)
	return n, err
}
func (r *capacityReadSeeker) Seek(offset int64, whence int) (int64, error) {
	r.credit = 0
	return r.r.Seek(offset, whence)
}

func receiveHeadroom(size int64) uint64 { return 2*uint64(size) + metadataHeadroom }

type StagingEntry struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Operation string    `json:"operation,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type stageFailure struct {
	Version   int       `json:"version"`
	Operation string    `json:"operation"`
	Error     string    `json:"error"`
	At        time.Time `json:"at"`
}

func (e *Engine) Staging() ([]StagingEntry, error) {
	unlock, err := e.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return e.staging()
}
func (e *Engine) staging() ([]StagingEntry, error) {
	es, err := os.ReadDir(filepath.Join(e.cfg.Root, "staging"))
	if err != nil {
		return nil, err
	}
	out := []StagingEntry{}
	for _, ent := range es {
		if !ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			return nil, err
		}
		entry := StagingEntry{Name: ent.Name(), State: "interrupted", UpdatedAt: info.ModTime()}
		var failure stageFailure
		if err := readRecord(filepath.Join(e.cfg.Root, "staging", ent.Name(), "failure.json"), &failure); err == nil && failure.Version == Version {
			entry.State = "failed"
			entry.Operation = failure.Operation
			entry.Error = failure.Error
			entry.UpdatedAt = failure.At
		} else if !os.IsNotExist(err) {
			entry.State = "unreadable-diagnostic"
			entry.Error = "staging failure record is unreadable"
		}
		out = append(out, entry)
	}
	return out, nil
}

func (e *Engine) newStage(prefix string) (string, error) {
	if err := e.checkCapacity(metadataHeadroom); err != nil {
		return "", err
	}
	entries, err := e.staging()
	if err != nil {
		return "", err
	}
	if len(entries) >= maxStagingEntries {
		return "", fmt.Errorf("recovery staging limit (%d) reached; inspect/delete retained staging before retrying", maxStagingEntries)
	}
	return os.MkdirTemp(filepath.Join(e.cfg.Root, "staging"), prefix)
}

// Keep failed snapshot/import stages identifiable without ever publishing them.
// Even if the diagnostic cannot be persisted, the returned error names the stage
// and Staging still reports the retained directory as interrupted.
func (e *Engine) recordStageFailure(stage, operation string, result *error) {
	if *result == nil {
		return
	}
	cause := *result
	err := writeRecord(filepath.Join(stage, "failure.json"), stageFailure{Version: Version, Operation: operation, Error: cause.Error(), At: e.cfg.Now().UTC()})
	*result = fmt.Errorf("recovery stage %s retained: %w", filepath.Base(stage), errors.Join(cause, err))
}

// DeleteStaging is an explicit authenticated cleanup of unpublished work only.
// No active cold/population operation may depend on the directory being removed.
func (e *Engine) DeleteStaging(ctx context.Context, name string) error {
	if !identifier.MatchString(name) {
		return errors.New("invalid staging identity")
	}
	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.requireIdle(); err != nil {
		return err
	}
	path := filepath.Join(e.cfg.Root, "staging", name)
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("staging entry is not a real directory")
	}
	if err := storage.RemoveTree(ctx, e.cfg.FS, path); err != nil {
		return err
	}
	return syncDir(filepath.Join(e.cfg.Root, "staging"))
}
