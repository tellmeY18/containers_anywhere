package recovery

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	MaxStreamBytes  int64 = 512 << 30
	MaxArchiveBytes int64 = 1 << 40
	MaxMembers            = 10000
)

type archiveMember struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type archiveManifest struct {
	Version int             `json:"version"`
	Point   Manifest        `json:"point"`
	Members []archiveMember `json:"members"`
}

// Export emits a full, self-contained tar, not a live disk copy. It spools send
// streams to know their exact size and hash before emitting the archive manifest.
// The host must encrypt/authenticate the complete stream before remote transfer.
func (e *Engine) Export(ctx context.Context, id string, w io.Writer) (result error) {
	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.requireIdle(); err != nil {
		return err
	}
	m, err := e.get(id)
	if err != nil {
		return err
	}
	if err := e.verify(ctx, e.pointPath(id), m); err != nil {
		return err
	}
	if len(m.Captures) > MaxMembers {
		return errors.New("too many captures")
	}
	dir, err := e.newStage("export-")
	if err != nil {
		return err
	}
	defer func() {
		if result != nil {
			e.recordStageFailure(dir, "export", &result)
		} else {
			_ = os.RemoveAll(dir)
		}
	}()
	a := archiveManifest{Version: Version, Point: m}
	var total int64
	for _, name := range m.Captures {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		h := sha256.New()
		bw := &boundedWriter{w: &capacityWriter{e: e, w: io.MultiWriter(f, h)}, remaining: MaxStreamBytes}
		err = e.cfg.FS.Send(ctx, filepath.Join(e.pointPath(id), name), bw)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
		size := MaxStreamBytes - bw.remaining
		total += size
		if total > MaxArchiveBytes {
			return errors.New("export exceeds archive size limit")
		}
		a.Members = append(a.Members, archiveMember{Name: name + ".btrfs", Size: size, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("archive manifest too large")
	}
	if err := e.checkCapacity(metadataHeadroom); err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	for i, member := range a.Members {
		if err := tw.WriteHeader(&tar.Header{Name: member.Name, Mode: 0600, Size: member.Size, Typeflag: tar.TypeReg, Format: tar.FormatGNU}); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(dir, m.Captures[i]))
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

type boundedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("stream exceeds size limit")
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// Import verifies the complete archive and all member hashes before receiving
// any subvolumes. Received snapshots remain staged until every member and the
// captured datastore/identity manifests validate. It never activates live data.
func (e *Engine) Import(ctx context.Context, r io.Reader) (point Manifest, result error) {
	unlock, err := e.lock()
	if err != nil {
		return Manifest{}, err
	}
	defer unlock()
	if err := e.requireIdle(); err != nil {
		return Manifest{}, err
	}
	tr := tar.NewReader(io.LimitReader(r, MaxArchiveBytes+(64<<20)))
	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, err
	}
	if hdr.Name != "manifest.json" || hdr.Typeflag != tar.TypeReg || hdr.Size < 1 || hdr.Size > 4<<20 || hdr.Linkname != "" {
		return Manifest{}, errors.New("archive must begin with a bounded regular manifest.json")
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		return Manifest{}, err
	}
	var a archiveManifest
	if err := decode(b, &a); err != nil {
		return Manifest{}, err
	}
	m := a.Point
	if a.Version != Version || len(a.Members) != len(m.Captures) || len(a.Members) > MaxMembers {
		return m, errors.New("archive capture count mismatch")
	}
	if err := e.validateManifest(m); err != nil {
		return m, err
	}
	if err := e.compatible(m); err != nil {
		return m, err
	}
	if _, err := os.Lstat(e.pointPath(m.ID)); !os.IsNotExist(err) {
		return m, errors.New("import destination point already exists or is inaccessible")
	}
	// Validate the WHOLE declaration before creating even the first spool.
	var total, largest int64
	for i, member := range a.Members {
		if member.Name != m.Captures[i]+".btrfs" || member.Size < 1 || member.Size > MaxStreamBytes || len(member.SHA256) != 64 {
			return m, errors.New("invalid archive member declaration")
		}
		if _, err := hex.DecodeString(member.SHA256); err != nil {
			return m, errors.New("invalid archive member digest")
		}
		if member.Size > MaxArchiveBytes-total {
			return m, errors.New("archive size limit exceeded")
		}
		total += member.Size
		if member.Size > largest {
			largest = member.Size
		}
	}
	// All streams coexist during authentication. Also leave room for the
	// largest receive's conservative data/metadata budget before spooling.
	if err := e.checkCapacity(uint64(total) + receiveHeadroom(largest)); err != nil {
		return m, err
	}
	dir, err := e.newStage("import-")
	if err != nil {
		return m, err
	}
	defer e.recordStageFailure(dir, "import", &result)
	streams := filepath.Join(dir, "streams")
	if err := os.Mkdir(streams, 0700); err != nil {
		return m, err
	}
	stage := filepath.Join(dir, "point")
	if err := os.Mkdir(stage, 0700); err != nil {
		return m, err
	}
	for i, member := range a.Members {
		hdr, err := tr.Next()
		if err != nil {
			return m, err
		}
		if hdr.Name != member.Name || hdr.Size != member.Size || hdr.Typeflag != tar.TypeReg || hdr.Linkname != "" || len(hdr.PAXRecords) > 0 {
			return m, errors.New("archive member name, type or size mismatch")
		}
		f, err := os.OpenFile(filepath.Join(streams, m.Captures[i]), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return m, err
		}
		h := sha256.New()
		n, err := io.Copy(&capacityWriter{e: e, w: io.MultiWriter(f, h)}, tr)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return m, err
		}
		if n != member.Size || hex.EncodeToString(h.Sum(nil)) != member.SHA256 {
			return m, errors.New("archive member checksum mismatch")
		}
	}
	if _, err := tr.Next(); err != io.EOF {
		return m, errors.New("extra or malformed archive member")
	}
	// Consume through the authenticated reader EOF. tar's zero blocks alone
	// cannot prove that an outer encryption stream authenticated successfully.
	if n, err := io.Copy(io.Discard, io.LimitReader(r, 1)); err != nil || n != 0 {
		return m, errors.New("trailing archive data or authentication failure")
	}
	for i, name := range m.Captures {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		if err := e.checkCapacity(receiveHeadroom(a.Members[i].Size)); err != nil {
			return m, err
		}
		f, err := os.Open(filepath.Join(streams, name))
		if err != nil {
			return m, err
		}
		receiveDir := filepath.Join(dir, "receive-"+name)
		if err := os.Mkdir(receiveDir, 0700); err != nil {
			f.Close()
			return m, err
		}
		err = e.cfg.FS.Receive(ctx, receiveDir, name, &capacityReadSeeker{e: e, r: f})
		err = errors.Join(err, f.Close())
		if err != nil {
			return m, err
		}
		if err := e.checkCapacity(metadataHeadroom); err != nil {
			return m, err
		}
		received := filepath.Join(receiveDir, name)
		// Cross-parent directory rename requires MAY_WRITE on the moved inode
		// (to update '..'), which Btrfs refuses for a received read-only root.
		// Create a read-only snapshot at its final staged name instead. Keep the
		// received source until that independent snapshot is durably synced.
		if err := e.cfg.FS.Snapshot(ctx, received, filepath.Join(stage, name), true); err != nil {
			return m, fmt.Errorf("stage received snapshot %s: %w", name, err)
		}
		if err := e.cfg.FS.Sync(ctx, stage); err != nil {
			return m, fmt.Errorf("sync staged snapshot %s: %w", name, err)
		}
		if err := e.cfg.FS.DeleteSubvolume(ctx, received); err != nil {
			return m, fmt.Errorf("remove duplicated received snapshot %s: %w", name, err)
		}
		// This authenticated stream now has a durable read-only staged copy.
		// Reclaim its redundant spool before receiving the next member.
		if err := os.Remove(filepath.Join(streams, name)); err != nil {
			return m, err
		}
	}
	if err := e.verify(ctx, stage, m); err != nil {
		return m, fmt.Errorf("import validation: %w", err)
	}
	if err := e.publish(ctx, stage, m); err != nil {
		return m, err
	}
	_ = os.RemoveAll(dir)
	return m, nil
}
