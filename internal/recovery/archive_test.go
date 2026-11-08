//go:build linux || darwin

package recovery

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type testArchiveEntry struct {
	header tar.Header
	body   []byte
}

func archiveEntries(t *testing.T, b []byte) []testArchiveEntry {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var entries []testArchiveEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, testArchiveEntry{*h, body})
	}
}
func archiveBytes(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		h := entry.header
		h.Size = int64(len(entry.body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestArchiveRejectsTraversalLinksExtrasAndIncompleteCaptureSet(t *testing.T) {
	f := setup(t)
	m := f.capture("archive-point", "full")
	var raw bytes.Buffer
	if err := f.e.Export(context.Background(), m.ID, &raw); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"traversal", "symlink", "extra", "incomplete", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			entries := archiveEntries(t, raw.Bytes())
			switch kind {
			case "traversal":
				entries[1].header.Name = "../escape"
			case "symlink":
				entries[1].header.Typeflag = tar.TypeSymlink
				entries[1].header.Linkname = "../escape"
				entries[1].body = nil
			case "extra":
				entries = append(entries, testArchiveEntry{header: tar.Header{Name: "extra", Mode: 0600, Typeflag: tar.TypeReg}, body: []byte("extra")})
			case "incomplete":
				var a archiveManifest
				if err := json.Unmarshal(entries[0].body, &a); err != nil {
					t.Fatal(err)
				}
				a.Point.Captures = a.Point.Captures[:len(a.Point.Captures)-1]
				a.Members = a.Members[:len(a.Members)-1]
				b, err := json.Marshal(a)
				if err != nil {
					t.Fatal(err)
				}
				entries[0].body = b
				entries = entries[:len(entries)-1]
			}
			b := archiveBytes(t, entries)
			if kind == "truncated" {
				b = b[:len(b)/2]
			}
			other := setup(t)
			if _, err := other.e.Import(context.Background(), bytes.NewReader(b)); err == nil {
				t.Fatal("malformed archive accepted")
			}
			if other.fs.received != 0 {
				t.Fatal("receive happened before complete archive validation")
			}
			if points, _ := other.e.List(); len(points) != 0 {
				t.Fatal("malformed archive published")
			}
		})
	}
}

func TestQueuedRestartExcludesExport(t *testing.T) {
	f := setup(t)
	m := f.capture("point", "full")
	if _, err := f.e.Submit(context.Background(), Request{ID: "next", Action: "capture", Scope: "full"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.e.Export(context.Background(), m.ID, &buf); err == nil {
		t.Fatal("export started while a restart was queued")
	}
	if buf.Len() != 0 {
		t.Fatal("partial export emitted after restart was queued")
	}
}

// Model the receive/publication contract rather than relying on ordinary host
// directory rename semantics: received roots are RO and may only be retired
// after an independently created RO snapshot has been synced at its final name.
type receivedPromotionBackend struct {
	Backend
	received     []string
	clones       map[string]string
	synced       map[string]bool
	deleted      []string
	failure      error
	failSnapshot bool
	failSync     bool
}

func (b *receivedPromotionBackend) Receive(ctx context.Context, dir, name string, r io.Reader) error {
	if err := b.Backend.Receive(ctx, dir, name, r); err != nil {
		return err
	}
	b.received = append(b.received, filepath.Join(dir, name))
	return nil
}

func (b *receivedPromotionBackend) Snapshot(ctx context.Context, src, dst string, readOnly bool) error {
	known := false
	for _, path := range b.received {
		known = known || src == path
	}
	ro, err := b.Backend.ReadOnly(ctx, src)
	if err != nil {
		return err
	}
	if !known || !ro || !readOnly || filepath.Base(filepath.Dir(dst)) != "point" || filepath.Base(dst) != filepath.Base(src) {
		return errors.New("import did not snapshot an unchanged RO received root into the final staging name")
	}
	if b.failSnapshot {
		return b.failure
	}
	if err := b.Backend.Snapshot(ctx, src, dst, true); err != nil {
		return err
	}
	b.clones[src] = dst
	return nil
}

func (b *receivedPromotionBackend) Sync(ctx context.Context, path string) error {
	for _, dst := range b.clones {
		if filepath.Dir(dst) == path && b.failSync {
			return b.failure
		}
	}
	if err := b.Backend.Sync(ctx, path); err != nil {
		return err
	}
	for _, dst := range b.clones {
		if filepath.Dir(dst) == path {
			b.synced[dst] = true
		}
	}
	return nil
}

func (b *receivedPromotionBackend) DeleteSubvolume(ctx context.Context, path string) error {
	dst, ok := b.clones[path]
	if !ok || !b.synced[dst] {
		return errors.New("received source was deleted before its replacement snapshot was durable")
	}
	ro, err := b.Backend.ReadOnly(ctx, path)
	if err != nil {
		return err
	}
	if !ro {
		return errors.New("received source read-only flag was changed")
	}
	if err := b.Backend.DeleteSubvolume(ctx, path); err != nil {
		return err
	}
	b.deleted = append(b.deleted, path)
	return nil
}

func TestImportSnapshotsReceivedReadOnlyRootBeforePublication(t *testing.T) {
	source, m, _ := populationFixture(t)
	var archive bytes.Buffer
	if err := source.e.Export(context.Background(), m.ID, &archive); err != nil {
		t.Fatal(err)
	}
	destination := setup(t)
	b := &receivedPromotionBackend{Backend: destination.fs, clones: map[string]string{}, synced: map[string]bool{}}
	destination.e.cfg.FS = b
	imported, err := destination.e.Import(context.Background(), bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.received) != len(m.Captures) || len(b.clones) != len(m.Captures) || len(b.deleted) != len(m.Captures) {
		t.Fatalf("import bypassed RO snapshot promotion: received=%v clones=%v deleted=%v", b.received, b.clones, b.deleted)
	}
	for _, name := range imported.Captures {
		path := filepath.Join(destination.e.pointPath(imported.ID), name)
		ro, err := destination.fs.ReadOnly(context.Background(), path)
		if err != nil || !ro {
			t.Fatalf("published capture is not RO: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(path, "data"))
		if err != nil || string(data) != "snapshot-volume" {
			t.Fatalf("imported content changed: %q %v", data, err)
		}
	}
}

func TestImportPromotionFailurePreservesReceivedReadOnlySource(t *testing.T) {
	source, m, _ := populationFixture(t)
	var archive bytes.Buffer
	if err := source.e.Export(context.Background(), m.ID, &archive); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"snapshot", "sync"} {
		t.Run(phase, func(t *testing.T) {
			destination := setup(t)
			injected := errors.New("injected promotion " + phase + " failure")
			b := &receivedPromotionBackend{Backend: destination.fs, clones: map[string]string{}, synced: map[string]bool{}, failure: injected, failSnapshot: phase == "snapshot", failSync: phase == "sync"}
			destination.e.cfg.FS = b
			if _, err := destination.e.Import(context.Background(), bytes.NewReader(archive.Bytes())); !errors.Is(err, injected) {
				t.Fatalf("expected injected failure: %v", err)
			}
			if len(b.received) != 1 || len(b.deleted) != 0 {
				t.Fatalf("received source was not preserved: received=%v deleted=%v", b.received, b.deleted)
			}
			path := b.received[0]
			ro, err := destination.fs.ReadOnly(context.Background(), path)
			if err != nil || !ro {
				t.Fatalf("source RO state changed: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(path, "data"))
			if err != nil || string(data) != "snapshot-volume" {
				t.Fatalf("received source content changed: %q %v", data, err)
			}
			if points, err := destination.e.List(); err != nil || len(points) != 0 {
				t.Fatalf("failed import published a point: %v %v", points, err)
			}
		})
	}
}
