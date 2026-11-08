//go:build linux || darwin

package recovery

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
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
