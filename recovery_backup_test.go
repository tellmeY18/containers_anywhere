package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/backuprepo"
	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

func runRecoveryBackupForTest(ctx context.Context, client *http.Client, o RecoveryBackupOptions) (any, error) {
	return runRecoveryBackupConfigured(ctx, client, o, func(r *backuprepo.Repository) {
		r.Config.DiskSpace = func(path string) (diskspace.Usage, error) {
			return diskspace.Usage{Path: path, Total: 100 << 30, Free: 80 << 30}, nil
		}
	})
}

func recoveryBackupFixture(t *testing.T) (string, string, string, backuprepo.Destination) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, repo, keys := filepath.Join(base, "state"), filepath.Join(base, "repo"), filepath.Join(base, "keys")
	for _, p := range []string{state, repo, keys} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(keys, "age.key")
	d, err := backuprepo.Enroll(state, backuprepo.EnrollmentOptions{Name: "Recovery", Path: repo, KeyPath: key})
	if err != nil {
		t.Fatal(err)
	}
	return state, repo, key, d
}

type recoveryBackupRoundTrip func(*http.Request) (*http.Response, error)

func (f recoveryBackupRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecoveryBackupGuestStreamingDropsOrdinaryClientDeadline(t *testing.T) {
	state, repo, _, d := recoveryBackupFixture(t)
	payload := strings.Repeat("full-volume", 10000)
	client := &http.Client{Timeout: time.Nanosecond, Transport: recoveryBackupRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "http://guest/_agent/recovery/points/point-1/export" {
			t.Fatalf("unexpected guest endpoint: %s", r.URL)
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) < 5*time.Hour {
			t.Fatal("ordinary short client timeout leaked into streaming export")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload)), ContentLength: int64(len(payload)), Header: make(http.Header)}, nil
	})}
	result, err := runRecoveryBackupForTest(context.Background(), client, RecoveryBackupOptions{Operation: "export", State: state, DestinationID: d.ID, PointID: "point-1", BackupID: "backup-1", Scope: "workload"})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := result.(backuprepo.Entry)
	if !ok || e.PlainBytes != int64(len(payload)) || e.Mode != "Full" {
		t.Fatalf("export result: %#v", result)
	}
	r, err := backuprepo.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	entries, err := r.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %v %v", entries, err)
	}
}

func TestRecoveryBackupClonesHeaderTimeoutWithoutChangingSharedTransport(t *testing.T) {
	original := &http.Transport{ResponseHeaderTimeout: 30 * time.Second}
	stream, _, _, err := recoveryBackupClient(&http.Client{Transport: original, Timeout: 30 * time.Second}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.CloseIdleConnections()
	clone, ok := stream.Transport.(*http.Transport)
	if !ok || clone == original || clone.ResponseHeaderTimeout != 0 || stream.Timeout != 0 {
		t.Fatal("streaming timeouts were not removed on an independent transport")
	}
	if original.ResponseHeaderTimeout != 30*time.Second {
		t.Fatal("ordinary agent transport was modified")
	}
}

func TestRecoveryBackupCLIControlBridgeAndIndependentImport(t *testing.T) {
	state, repo, key, d := recoveryBackupFixture(t)
	payload := strings.Repeat("guest-full-btrfs-archive\x00", 10000)
	var imports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer host-only-token" {
			t.Error("missing host control capability")
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /control/recovery/points/point-1/export":
			_, _ = io.WriteString(w, payload)
		case "POST /control/recovery/import":
			imports.Add(1)
			b, err := io.ReadAll(r.Body)
			if err != nil || string(b) != payload {
				t.Errorf("guest payload mismatch: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"imported":"point-1"}`)
		default:
			t.Errorf("unexpected control route: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	writeControl := func(state string) {
		b, err := json.Marshal(control{URL: server.URL, Token: "host-only-token"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "control.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeControl(state)
	_, err := runRecoveryBackupForTest(context.Background(), nil, RecoveryBackupOptions{Operation: "export", State: state, DestinationID: d.ID, PointID: "point-1", BackupID: "backup-1"})
	if err != nil {
		t.Fatal(err)
	}
	// Restore to a new running appliance using explicit repo+key only, after
	// removing the original installation's enrollment and control state.
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	writeControl(state)
	staging, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := RecoveryBackupOptions{Operation: "import", State: state, Repository: repo, KeyPath: key, BackupID: "backup-1", StagingDir: staging}
	if _, err := runRecoveryBackupForTest(context.Background(), nil, o); err != nil {
		t.Fatal(err)
	}
	if imports.Load() != 1 {
		t.Fatal("verified import did not reach new guest")
	}
	// Corrupt the ciphertext. No import HTTP request, even with its first few
	// chunks, may reach the guest until the entire age stream authenticates.
	name := filepath.Join(repo, "backup-1.age")
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if err := os.WriteFile(name, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runRecoveryBackupForTest(context.Background(), nil, o); err == nil {
		t.Fatal("tampered backup imported")
	}
	if imports.Load() != 1 {
		t.Fatal("partially authenticated plaintext reached guest")
	}
	files, err := os.ReadDir(staging)
	if err != nil || len(files) != 0 {
		t.Fatalf("plaintext stage leaked: %v %v", files, err)
	}
}

func TestRecoveryBackupControlRefusesRedirectAndRemoteAddress(t *testing.T) {
	state, _, _, d := recoveryBackupFixture(t)
	var leaked atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	for _, address := range []string{redirect.URL, "http://example.org:8484"} {
		b, err := json.Marshal(control{URL: address, Token: "host-only-token"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "control.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = runRecoveryBackupForTest(context.Background(), nil, RecoveryBackupOptions{Operation: "export", State: state, DestinationID: d.ID, PointID: "point", BackupID: "blocked"})
		if err == nil {
			t.Fatalf("unsafe address/redirect accepted: %s", address)
		}
	}
	if leaked.Load() != 0 {
		t.Fatal("control token followed a redirect")
	}
}
