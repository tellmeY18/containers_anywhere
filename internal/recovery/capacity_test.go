//go:build linux || darwin

package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

func fixedSpace(total, free uint64) func(string) (diskspace.Usage, error) {
	return func(path string) (diskspace.Usage, error) {
		return diskspace.Usage{Path: path, Total: total, Free: free}, nil
	}
}

func TestCapacityReserveAndHTTP(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct{ total, reserve uint64 }{{8 << 30, 1 << 30}, {20 << 30, 1 << 30}, {100 << 30, 5 << 30}, {(100 << 30) + 1, (5 << 30) + 1}} {
		f.e.cfg.DiskSpace = fixedSpace(tc.total, tc.total/2)
		got, err := f.e.Capacity()
		if err != nil || got.SizeBytes != tc.total || got.FreeBytes != tc.total/2 || got.ReserveBytes != tc.reserve {
			t.Fatalf("capacity=%+v err=%v", got, err)
		}
	}
	mux := http.NewServeMux()
	f.e.Routes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/_agent/recovery/capacity", nil))
	var got Capacity
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || got.ReserveBytes != (5<<30)+1 {
		t.Fatalf("capacity route: %d %s %v", w.Code, w.Body.String(), err)
	}
}

func TestCapacityFailureBeforeNewOperationsPreservesSource(t *testing.T) {
	f := setup(t)
	f.e.cfg.DiskSpace = fixedSpace(20<<30, 1<<30)
	if _, err := f.e.Submit(context.Background(), Request{ID: "no-room", Action: "capture", Scope: "full"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("submission ignored floor: %v", err)
	}
	ops, err := f.e.Operations()
	if err != nil || len(ops) != 0 {
		t.Fatal(ops, err)
	}
	stages, err := f.e.Staging()
	if err != nil || len(stages) != 0 {
		t.Fatal(stages, err)
	}
	if f.read("database") != "original-db" {
		t.Fatal("source changed")
	}
}

func TestCapacityLossAfterQueueIsTerminalBeforeDestructiveWork(t *testing.T) {
	for _, action := range []string{"capture", "restore"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t)
			req := Request{ID: "queued", Action: action, Scope: "full"}
			if action == "restore" {
				m := f.capture("source", "full")
				req.PointID = m.ID
			}
			f.write("database", "current-db")
			if _, err := f.e.Submit(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			f.e.cfg.DiskSpace = fixedSpace(20<<30, 1<<30)
			if err := f.e.ProcessCold(context.Background()); err != nil {
				t.Fatal(err)
			}
			op, err := f.e.Status(req.ID)
			if err != nil || op.Phase != "failed" || !strings.Contains(op.Error, ErrCapacity.Error()) {
				t.Fatal(op, err)
			}
			if f.read("database") != "current-db" || op.SafetyID != "" {
				t.Fatal("low-headroom operation changed source", op)
			}
		})
	}
}

func TestLowCapacityRefusesRegistrationAndPopulation(t *testing.T) {
	f, m, req := populationFixture(t)
	f.e.cfg.DiskSpace = fixedSpace(20<<30, 1<<30)
	if _, err := f.e.RegisterSnapshots(context.Background(), RegisterRequest{ID: "next-registration", Members: m.Members}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("registration ignored floor: %v", err)
	}
	if _, err := f.e.PopulateWorkload(context.Background(), req); !errors.Is(err, ErrCapacity) {
		t.Fatalf("population ignored floor: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(f.e.cfg.PoolDir, "volumes/new-volume"))
	if err != nil || len(entries) != 0 {
		t.Fatal("target changed", entries, err)
	}
	if _, err := f.e.Get(m.ID); err != nil {
		t.Fatal("last good point lost", err)
	}
}

func TestDestructiveRestoreAndRollbackResumeWithoutFloorGate(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprint(rollback), func(t *testing.T) {
			f := setup(t)
			m := f.capture("source", "full")
			f.write("database", "current-db")
			if _, err := f.e.Submit(context.Background(), Request{ID: "restore", Action: "restore", Scope: "full", PointID: m.ID}); err != nil {
				t.Fatal(err)
			}
			copy := f.e.cfg.Copy
			if rollback {
				f.e.cfg.Copy = func(context.Context, string, string) error { return errors.New("copy failed") }
				if err := f.e.ProcessCold(context.Background()); !errors.Is(err, ErrInconsistent) {
					t.Fatal(err)
				}
			} else {
				f.e.cfg.Copy = func(context.Context, string, string) error { panic("power loss") }
				func() {
					defer func() {
						if recover() == nil {
							t.Error("fault not injected")
						}
					}()
					_ = f.e.ProcessCold(context.Background())
				}()
			}
			f.e.cfg.Copy = copy
			f.e.cfg.DiskSpace = func(string) (diskspace.Usage, error) {
				t.Error("already-destructive resume consulted the new-operation floor")
				return diskspace.Usage{}, ErrCapacity
			}
			if err := f.e.ProcessCold(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := "original-db"
			if rollback {
				want = "current-db"
			}
			if f.read("database") != want {
				t.Fatal("resume did not restore the expected state")
			}
		})
	}
}

type decrementWriter struct {
	w    io.Writer
	free *uint64
}

func (w decrementWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	*w.free -= uint64(n)
	return n, err
}

func TestGuardedSpoolingChecksEveryMiBEvenForLargeWrite(t *testing.T) {
	f := setup(t)
	free := uint64(1<<30) + 2*capacityQuantum
	f.e.cfg.DiskSpace = func(path string) (diskspace.Usage, error) {
		return diskspace.Usage{Path: path, Total: 20 << 30, Free: free}, nil
	}
	var dst bytes.Buffer
	w := &capacityWriter{e: f.e, w: decrementWriter{w: &dst, free: &free}}
	n, err := w.Write(make([]byte, 3*capacityQuantum))
	if !errors.Is(err, ErrCapacity) || n != int(2*capacityQuantum) || dst.Len() != n || free != 1<<30 {
		t.Fatalf("guard: n=%d free=%d err=%v", n, free, err)
	}
}

type pressureBackend struct {
	Backend
	send     func(context.Context, string, io.Writer) error
	receive  func(context.Context, string, string, io.Reader) error
	snapshot func(context.Context, string, string, bool) error
}

func (b pressureBackend) Send(ctx context.Context, p string, w io.Writer) error {
	if b.send != nil {
		return b.send(ctx, p, w)
	}
	return b.Backend.Send(ctx, p, w)
}
func (b pressureBackend) Receive(ctx context.Context, p, n string, r io.Reader) error {
	if b.receive != nil {
		return b.receive(ctx, p, n, r)
	}
	return b.Backend.Receive(ctx, p, n, r)
}
func (b pressureBackend) Snapshot(ctx context.Context, s, d string, ro bool) error {
	if b.snapshot != nil {
		return b.snapshot(ctx, s, d, ro)
	}
	return b.Backend.Snapshot(ctx, s, d, ro)
}

func TestCapturePressureBeforePublicationLeavesOnlyFailedStaging(t *testing.T) {
	f := setup(t)
	pressure := false
	f.e.cfg.DiskSpace = func(path string) (diskspace.Usage, error) {
		free := uint64(10 << 30)
		if pressure {
			free = 1 << 30
		}
		return diskspace.Usage{Path: path, Total: 20 << 30, Free: free}, nil
	}
	f.e.cfg.FS = pressureBackend{Backend: f.fs, snapshot: func(ctx context.Context, s, d string, ro bool) error {
		err := f.fs.Snapshot(ctx, s, d, ro)
		pressure = true
		return err
	}}
	if _, err := f.e.Submit(context.Background(), Request{ID: "capture-pressure", Action: "capture", Scope: "full"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.ProcessCold(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, err := f.e.Status("capture-pressure")
	if err != nil || op.Phase != "failed" {
		t.Fatal(op, err)
	}
	points, _ := f.e.List()
	if len(points) != 0 {
		t.Fatal("partial point published")
	}
	stages, _ := f.e.Staging()
	if len(stages) != 1 || stages[0].State != "failed" {
		t.Fatal(stages)
	}
	if f.read("database") != "original-db" {
		t.Fatal("capture changed original")
	}
}

func stagingBytes(t *testing.T, root string, onlyStreams bool) (uint64, bool) {
	t.Helper()
	var size uint64
	found := false
	err := filepath.WalkDir(filepath.Join(root, "staging"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if onlyStreams && !strings.Contains(p, string(filepath.Separator)+"streams"+string(filepath.Separator)) {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if st.Mode().IsRegular() {
			found = true
			size += uint64(st.Size())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return size, found
}

func TestExportPressureRetainsDiagnosticWithoutEmittingArchive(t *testing.T) {
	f := setup(t)
	m := f.capture("source", "full")
	pressure := false
	f.e.cfg.DiskSpace = func(path string) (diskspace.Usage, error) {
		free := uint64(10 << 30)
		if pressure {
			used, _ := stagingBytes(t, f.e.cfg.Root, false)
			free = (1 << 30) + 2*capacityQuantum - used
		}
		return diskspace.Usage{Path: path, Total: 20 << 30, Free: free}, nil
	}
	f.e.cfg.FS = pressureBackend{Backend: f.fs, send: func(_ context.Context, _ string, w io.Writer) error {
		pressure = true
		_, err := w.Write(make([]byte, 3*capacityQuantum))
		return err
	}}
	var output bytes.Buffer
	if err := f.e.Export(context.Background(), m.ID, &output); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("capacity-failed export emitted an archive")
	}
	stages, err := f.e.Staging()
	if err != nil || len(stages) != 1 || stages[0].State != "failed" || !strings.Contains(stages[0].Error, ErrCapacity.Error()) {
		t.Fatal(stages, err)
	}
	if err := f.e.Export(context.Background(), m.ID, &output); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	again, _ := f.e.Staging()
	if len(again) != 1 {
		t.Fatal("retry accumulated another insufficient-space stage")
	}
	if _, err := f.e.Get(m.ID); err != nil {
		t.Fatal("last good point lost", err)
	}
}

func exportFixture(t *testing.T, large bool) []byte {
	t.Helper()
	f := setup(t)
	if large {
		f.write("btrfs-csi-pool/volumes/v1/data", strings.Repeat("x", 3<<20))
	}
	m := f.capture("archive-source", "full")
	var raw bytes.Buffer
	if err := f.e.Export(context.Background(), m.ID, &raw); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func TestImportHeadroomPreflightAndStreamingPressure(t *testing.T) {
	raw := exportFixture(t, true)
	t.Run("declared-total", func(t *testing.T) {
		f := setup(t)
		f.e.cfg.DiskSpace = fixedSpace(20<<30, (1<<30)+metadataHeadroom)
		if _, err := f.e.Import(context.Background(), bytes.NewReader(raw)); !errors.Is(err, ErrCapacity) {
			t.Fatal(err)
		}
		stages, _ := f.e.Staging()
		if len(stages) != 0 || f.fs.received != 0 {
			t.Fatal("preflight wrote staging or received data")
		}
	})
	t.Run("spool-pressure", func(t *testing.T) {
		f := setup(t)
		f.e.cfg.DiskSpace = func(path string) (diskspace.Usage, error) {
			used, found := stagingBytes(t, f.e.cfg.Root, true)
			free := uint64(10 << 30)
			if found {
				free = (1 << 30) + 2*capacityQuantum - used
			}
			return diskspace.Usage{Path: path, Total: 20 << 30, Free: free}, nil
		}
		if _, err := f.e.Import(context.Background(), bytes.NewReader(raw)); !errors.Is(err, ErrCapacity) {
			t.Fatal(err)
		}
		if f.fs.received != 0 {
			t.Fatal("receive preceded complete guarded spooling")
		}
		points, _ := f.e.List()
		if len(points) != 0 {
			t.Fatal("failed import published a point")
		}
		stages, _ := f.e.Staging()
		if len(stages) != 1 || stages[0].State != "failed" {
			t.Fatal(stages)
		}
	})
	t.Run("receive-pressure", func(t *testing.T) {
		f := setup(t)
		pressure := false
		f.e.cfg.DiskSpace = func(path string) (diskspace.Usage, error) {
			free := uint64(10 << 30)
			if pressure {
				free = 1 << 30
			}
			return diskspace.Usage{Path: path, Total: 20 << 30, Free: free}, nil
		}
		f.e.cfg.FS = pressureBackend{Backend: f.fs, receive: func(ctx context.Context, p, n string, r io.Reader) error {
			pressure = true
			return f.fs.Receive(ctx, p, n, r)
		}}
		if _, err := f.e.Import(context.Background(), bytes.NewReader(raw)); !errors.Is(err, ErrCapacity) {
			t.Fatal(err)
		}
		points, _ := f.e.List()
		if len(points) != 0 {
			t.Fatal("failed receive published a point")
		}
		if f.read("database") != "original-db" {
			t.Fatal("import changed live state")
		}
	})
}

func TestFailedStagingIsBoundedAndExplicitlyCleanable(t *testing.T) {
	f := setup(t)
	m := f.capture("source", "full")
	for i := 0; i < maxStagingEntries; i++ {
		if err := os.Mkdir(filepath.Join(f.e.cfg.Root, "staging", fmt.Sprintf("failed-%d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := f.e.Export(context.Background(), m.ID, &output); err == nil || !strings.Contains(err.Error(), "staging limit") {
		t.Fatal(err)
	}
	if err := f.e.DeleteStaging(context.Background(), "failed-0"); err != nil {
		t.Fatal(err)
	}
	stages, _ := f.e.Staging()
	if len(stages) != maxStagingEntries-1 {
		t.Fatal("staging cleanup failed")
	}
	if _, err := f.e.Get(m.ID); err != nil {
		t.Fatal("cleanup affected published point", err)
	}
}
