package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/backuprepo"
)

// RecoveryBackupOptions belongs to the host launcher. Repository/KeyPath must
// never be accepted from Kubernetes requests: controllers use DestinationID.
type RecoveryBackupOptions struct {
	Operation     string
	State         string
	DestinationID string
	Repository    string
	Name          string
	Recipient     string
	KeyPath       string
	PointID       string
	BackupID      string
	Scope         string
	StagingDir    string
	KeepLast      int
	MinFreeBytes  uint64
	Protected     bool
}

// runRecoveryBackup is reusable by authenticated launcher control callbacks.
// A supplied client uses the authenticated VM transport and fixed agent routes;
// nil uses the running launcher's loopback control bridge for CLI operations.
func runRecoveryBackup(ctx context.Context, client *http.Client, o RecoveryBackupOptions) (any, error) {
	return runRecoveryBackupConfigured(ctx, client, o, nil)
}

// Per-call dependency setup keeps tests independent of runner disk capacity;
// production entrypoints always use the real host capacity provider.
func runRecoveryBackupConfigured(ctx context.Context, client *http.Client, o RecoveryBackupOptions, configure func(*backuprepo.Repository)) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	switch o.Operation {
	case "destination-add":
		return backuprepo.Enroll(o.State, backuprepo.EnrollmentOptions{Name: o.Name, Path: o.Repository, Recipient: o.Recipient, KeyPath: o.KeyPath, KeepLast: o.KeepLast, MinFreeBytes: o.MinFreeBytes})
	case "destination-list":
		return backuprepo.Destinations(o.State)
	case "destination-remove":
		return nil, backuprepo.Forget(o.State, o.DestinationID)
	case "list", "verify", "import", "export", "protect", "prune":
	default:
		return nil, errors.New("recovery-backup operation must be destination add/list/remove, export, list, verify, import, protect or prune")
	}
	var repo *backuprepo.Repository
	var err error
	if o.Repository != "" {
		if o.DestinationID != "" {
			return nil, errors.New("choose --repository or --destination, not both")
		}
		repo, err = backuprepo.Open(o.Repository)
	} else {
		repo, err = backuprepo.Resolve(o.State, o.DestinationID)
	}
	if err != nil {
		return nil, err
	}
	defer repo.Close()
	if configure != nil {
		configure(repo)
	}
	switch o.Operation {
	case "list":
		return repo.List()
	case "verify":
		return repo.Verify(ctx, o.BackupID, o.KeyPath)
	case "protect":
		return nil, repo.Protect(o.BackupID, o.Protected)
	case "prune":
		return nil, repo.Prune()
	}
	stream, base, token, err := recoveryBackupClient(client, o.State)
	if err != nil {
		return nil, err
	}
	defer stream.CloseIdleConnections()
	if o.Operation == "export" {
		if !backuprepo.ValidID(o.PointID) {
			return nil, errors.New("--point must be a completed guest recovery point ID")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/points/"+o.PointID+"/export", nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := stream.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, recoveryBackupResponseError(resp)
		}
		// The guest holds its point/export lock for this response's lifetime;
		// host retention only operates after the complete body is durable.
		entry, err := repo.Export(ctx, resp.Body, backuprepo.ExportOptions{ID: o.BackupID, PointID: o.PointID, Scope: o.Scope, Protected: o.Protected, ExpectedBytes: resp.ContentLength})
		if entry.ID == "" {
			return nil, err
		}
		return entry, err
	}
	stagingDir := o.StagingDir
	if stagingDir == "" {
		stagingDir, err = filepath.EvalSymlinks(os.TempDir())
		if err != nil {
			return nil, err
		}
	}
	stage, err := repo.Stage(ctx, o.BackupID, o.KeyPath, stagingDir)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/import", stage.File)
	if err != nil {
		return nil, err
	}
	req.ContentLength = stage.Entry.PlainBytes
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := stream.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, recoveryBackupResponseError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("guest import response is too large")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{"imported": stage.Entry.ID}, nil
	}
	if !json.Valid(b) {
		return nil, errors.New("guest import response is not JSON")
	}
	return json.RawMessage(b), nil
}

func recoveryBackupResponseError(resp *http.Response) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return err
	}
	return fmt.Errorf("recovery transfer HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

func recoveryBackupClient(client *http.Client, state string) (*http.Client, string, string, error) {
	base, token := "http://guest/_agent/recovery", ""
	var transport http.RoundTripper
	if client != nil {
		transport = client.Transport
		if transport == nil {
			return nil, "", "", errors.New("recovery guest client requires an explicit VM transport")
		}
		if t, ok := transport.(*http.Transport); ok {
			clone := t.Clone()
			clone.ResponseHeaderTimeout = 0
			transport = clone
		}
	} else {
		c, err := readControl(state)
		if err != nil {
			return nil, "", "", errors.New("start a compatible recovery appliance for this state before export/import")
		}
		u, err := url.Parse(c.URL)
		if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || c.Token == "" {
			return nil, "", "", errors.New("invalid launcher control address")
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, "", "", errors.New("recovery control must be a literal loopback address")
		}
		base = strings.TrimSuffix(c.URL, "/") + "/control/recovery"
		token = c.Token
		transport = &http.Transport{Proxy: nil}
	}
	// Large full exports/imports are bounded by caller cancellation, not the
	// ordinary agent client's 30-second total timeout. Never follow redirects.
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base, token, nil
}

// runRecoveryBackupCLI owns its flags so main only needs an early dispatch.
func runRecoveryBackupCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: recovery-backup destination add|list|remove, export, list, verify, import, protect, prune [flags]")
	}
	op := args[0]
	args = args[1:]
	if op == "destination" {
		if len(args) == 0 {
			return errors.New("usage: recovery-backup destination add|list|remove [flags]")
		}
		op = "destination-" + args[0]
		args = args[1:]
	}
	home, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	o := RecoveryBackupOptions{Operation: op}
	fs := flag.NewFlagSet("recovery-backup "+op, flag.ContinueOnError)
	fs.StringVar(&o.State, "state", filepath.Join(home, "containers-anywhere"), "host enrollment / running recovery appliance state directory")
	fs.StringVar(&o.DestinationID, "destination", "", "opaque enrolled destination ID")
	fs.StringVar(&o.Repository, "repository", "", "existing absolute repository directory; usable without original state")
	fs.StringVar(&o.Repository, "path", "", "alias of --repository for destination add")
	fs.StringVar(&o.Name, "name", "", "host display name for destination add")
	fs.StringVar(&o.Recipient, "recipient", "", "public age X25519 recipient (export requires no private key)")
	fs.StringVar(&o.KeyPath, "key", "", "separate absolute age identity path; destination add creates it exclusively")
	fs.StringVar(&o.PointID, "point", "", "completed guest RecoveryPoint ID to export")
	fs.StringVar(&o.BackupID, "id", "", "backup ID for list/verify/import/protect; optional new export ID")
	fs.StringVar(&o.Scope, "scope", "system", "recovery point scope label (system, workload or full); payload stays opaque")
	fs.StringVar(&o.StagingDir, "staging-dir", "", "existing absolute directory for authenticated private import staging")
	fs.IntVar(&o.KeepLast, "keep", 8, "maximum retained backup count; protected and last verified points block pruning")
	fs.Uint64Var(&o.MinFreeBytes, "min-free-bytes", backuprepo.DefaultFreeFloor, "free-space reserve; effective minimum is max(configured, 20 GiB, 10% of volume)")
	fs.BoolVar(&o.Protected, "protected", false, "protect new backup from retention; for protect, set desired boolean")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments after recovery-backup operation")
	}
	o.State, err = filepath.Abs(o.State)
	if err != nil {
		return err
	}
	// Cluster state paths elsewhere in the launcher may use OS aliases (e.g.
	// /var on macOS); repositories and explicit key paths remain strict.
	if real, e := filepath.EvalSymlinks(o.State); e == nil {
		o.State = real
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	result, err := runRecoveryBackup(ctx, nil, o)
	if entry, ok := result.(backuprepo.Entry); ok && entry.ID == "" {
		result = nil
	}
	if destination, ok := result.(backuprepo.Destination); ok && destination.ID == "" {
		result = nil
	}
	if result != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if writeErr := enc.Encode(result); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	return err
}
