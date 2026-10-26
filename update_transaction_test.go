package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/protocol"
)

func updateFixture(t *testing.T) (state, old, next string) {
	t.Helper()
	state = t.TempDir()
	old, m := testBundle(t)
	next, n := testBundle(t)
	n.Version = "0.1.1"
	n.System = "/nix/store/current-candidate"
	mustSave(t, filepath.Join(next, "manifest.json"), n)
	mustSave(t, filepath.Join(state, "release.json"), m)
	for name, data := range map[string]string{
		"data.img":     "current cluster identity, PVC data and Kite credentials",
		"gateway.json": "{}",
		"gateway/caddy/pki/authorities/local/root.key": "private CA identity",
	} {
		path := filepath.Join(state, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "darwin" {
		dir := filepath.Join(filepath.Dir(old), "qemu")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		name := "qemu-system-x86_64"
		if runtime.GOARCH == "arm64" {
			name = "qemu-system-aarch64"
		}
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("runtime"), 0700); err != nil {
			t.Fatal(err)
		}
		mustSave(t, filepath.Join(dir, "runtime.json"), map[string]string{name: sha256Text([]byte("runtime"))})
	}
	return
}

func mustSave(t *testing.T, p string, v any) {
	t.Helper()
	if err := saveJSON(p, v); err != nil {
		t.Fatal(err)
	}
}

func requireUnchanged(t *testing.T, state, old string) {
	t.Helper()
	m, err := readManifest(filepath.Join(state, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := readManifest(filepath.Join(old, "manifest.json"))
	if !compatibleData(m, original) {
		t.Fatal("original selection changed")
	}
	b, err := os.ReadFile(filepath.Join(state, "data.img"))
	if err != nil || string(b) != "current cluster identity, PVC data and Kite credentials" {
		t.Fatal("original disk changed", err)
	}
}

func TestUpdatePrepareEveryDurableBoundary(t *testing.T) {
	state, old, next := updateFixture(t)
	var boundaries []string
	if err := prepareUpdate(state, next, old, updateHooks{boundary: func(s string) error {
		boundaries = append(boundaries, s)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	j, err := readUpdate(state)
	if err != nil {
		t.Fatal(err)
	}
	if checkUpdateStart(filepath.Join(updateRoot(state, j), "state")) == nil {
		t.Fatal("cold recovery point could be booted and modified in place")
	}
	for index, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			state, old, next := updateFixture(t)
			seen := 0
			err := prepareUpdate(state, next, old, updateHooks{boundary: func(string) error {
				current := seen
				seen++
				if current == index {
					return errors.New("interrupted")
				}
				return nil
			}})
			if err == nil {
				t.Fatal("interruption not injected")
			}
			requireUnchanged(t, state, old)
			if checkUpdateStart(state) == nil {
				t.Fatal("ordinary startup bypassed pending transaction")
			}
			if err := cancelUpdate(state, updateHooks{}); err != nil {
				t.Fatal(err)
			}
			if err := checkUpdateStart(state); err != nil {
				t.Fatal(err)
			}
			requireUnchanged(t, state, old)
		})
	}
}

func TestUpdatePublicationEveryDurableBoundary(t *testing.T) {
	for _, boundary := range []string{"activating", "selection-published", "receipt-removed", "candidate-running"} {
		t.Run(boundary, func(t *testing.T) {
			state, old, next := updateFixture(t)
			if err := prepareUpdate(state, next, old, updateHooks{}); err != nil {
				t.Fatal(err)
			}
			_, err := publishUpdate(state, updateHooks{boundary: func(s string) error {
				if s == boundary {
					return errors.New("crash after " + s)
				}
				return nil
			}})
			if err == nil || checkUpdateStart(state) == nil {
				t.Fatal("crash did not fence startup", err)
			}
			if cancelUpdate(state, updateHooks{}) == nil {
				t.Fatal("ambiguous execution allowed cancellation")
			}
			// Model candidate writes, including a failed boot. Retry must never
			// rewind to the older snapshot, even if no health response was seen.
			if err := os.WriteFile(filepath.Join(state, "data.img"), []byte("candidate writes"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := publishUpdate(state, updateHooks{}); err != nil {
				t.Fatal("explicit retry failed", err)
			}
			b, _ := os.ReadFile(filepath.Join(state, "data.img"))
			if string(b) != "candidate writes" {
				t.Fatal("retry rewound data")
			}
			dest := filepath.Join(t.TempDir(), "recovered")
			if err := recoverUpdate(state, dest, updateHooks{}); err != nil {
				t.Fatal(err)
			}
			requireUnchanged(t, dest, old)
			b, _ = os.ReadFile(filepath.Join(state, "data.img"))
			if string(b) != "candidate writes" {
				t.Fatal("recovery destroyed failed/current disk")
			}
			ca, _ := os.ReadFile(filepath.Join(dest, "gateway/caddy/pki/authorities/local/root.key"))
			if string(ca) != "private CA identity" {
				t.Fatal("recovery lost CA identity")
			}
			if err := recoverUpdate(state, dest, updateHooks{}); err == nil {
				t.Fatal("overwrote recovery destination")
			}
		})
	}
}

func TestUpdatePreflightFailures(t *testing.T) {
	for _, fault := range []string{"space", "corrupt", "contract", "k3s", "kite", "protocol", "arch", "symlink"} {
		t.Run(fault, func(t *testing.T) {
			state, old, next := updateFixture(t)
			h := updateHooks{}
			m, _ := readManifest(filepath.Join(next, "manifest.json"))
			switch fault {
			case "space":
				h.free = func(string) (uint64, error) { return 1, nil }
			case "corrupt":
				os.WriteFile(filepath.Join(next, "system.img"), []byte("corrupt"), 0600)
			case "contract":
				m.DataContract = ""
			case "k3s":
				m.K3s = "new-schema"
			case "kite":
				m.Kite = "new-schema"
			case "protocol":
				m.Protocol++
			case "arch":
				m.Arch = "unsupported"
			case "symlink":
				if runtime.GOOS == "windows" {
					t.Skip("unprivileged Windows symlink creation is not available")
				}
				os.Remove(filepath.Join(next, "system.img"))
				if err := os.Symlink(filepath.Join(old, "system.img"), filepath.Join(next, "system.img")); err != nil {
					t.Fatal(err)
				}
			}
			mustSave(t, filepath.Join(next, "manifest.json"), m)
			if err := prepareUpdate(state, next, old, h); err == nil {
				t.Fatal("accepted unsafe update")
			}
			requireUnchanged(t, state, old)
		})
	}
}

func TestUpdateExclusionAndExplicitAcknowledgements(t *testing.T) {
	state, _, _ := updateFixture(t)
	unlock, err := lockState(state)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := updateLock(state, false); err == nil {
		t.Fatal("concurrent update acquired lifecycle lock")
	}
	for _, op := range []string{"activate", "commit"} {
		if err := runUpdate(state, op, "", "", "", 0, true, false, false, false); err == nil {
			t.Fatal("missing explicit acknowledgement accepted", op)
		}
	}
}

func TestFailedCandidateBootRetainsCurrentDiskAndBlocksOrdinaryStart(t *testing.T) {
	state, old, next := updateFixture(t)
	if err := prepareUpdate(state, next, old, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_QEMU_ACCEL", "tcg")
	originalLaunch := launchVM
	launchVM = func(_, state string, _ manifest) (machine, error) {
		if unlock, err := lockState(state); err == nil {
			unlock()
			t.Fatal("activation released lifecycle lock before boot")
		}
		if err := os.WriteFile(filepath.Join(state, "data.img"), []byte("failed candidate writes"), 0600); err != nil {
			t.Fatal(err)
		}
		return nil, errors.New("candidate boot failed after possible writes")
	}
	t.Cleanup(func() { launchVM = originalLaunch })
	if err := serveUpdate(state, "", 0, true, true); err == nil {
		t.Fatal("failed candidate reported success")
	}
	j, err := readUpdate(state)
	if err != nil || j.Phase != "candidate-running" {
		t.Fatal("lost ambiguous-execution journal", err)
	}
	if checkUpdateStart(state) == nil || cancelUpdate(state, updateHooks{}) == nil {
		t.Fatal("failed boot enabled ordinary startup or rewind")
	}
	b, _ := os.ReadFile(filepath.Join(state, "data.img"))
	if string(b) != "failed candidate writes" {
		t.Fatal("failed candidate disk was rewound")
	}
	dest := filepath.Join(t.TempDir(), "recovered")
	if err := recoverUpdate(state, dest, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	requireUnchanged(t, dest, old)
}

func TestUpdateResumeAndRecoveryIntegrity(t *testing.T) {
	state, old, next := updateFixture(t)
	if err := prepareUpdate(state, next, old, updateHooks{boundary: func(s string) error {
		if s == "retained:candidate/kernel" {
			return errors.New("interrupted copy")
		}
		return nil
	}}); err == nil {
		t.Fatal("missing interruption")
	}
	if err := prepareUpdate(state, next, old, updateHooks{}); err != nil {
		t.Fatal("resume failed", err)
	}
	j, _ := readUpdate(state)
	if err := os.WriteFile(filepath.Join(updateRoot(state, j), "state/data.img"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := publishUpdate(state, updateHooks{}); err == nil {
		t.Fatal("activated without a viable recovery point")
	}
	requireUnchanged(t, state, old)
}

func TestRecoveryRetainsRecordedRuntimeInsteadOfStaleSibling(t *testing.T) {
	state, old, next := updateFixture(t)
	m, _ := readManifest(filepath.Join(old, "manifest.json"))
	dir := t.TempDir()
	name := "qemu-system-x86_64"
	if runtime.GOARCH == "arm64" {
		name = "qemu-system-aarch64"
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	data := []byte("independently updated app runtime")
	if err := os.WriteFile(filepath.Join(dir, name), data, 0700); err != nil {
		t.Fatal(err)
	}
	mustSave(t, filepath.Join(dir, "runtime.json"), map[string]string{name: sha256Text(data)})
	mustSave(t, filepath.Join(state, "runtime-selection.json"), selectedRuntime{bundleID(m), dir})
	if selected, err := recoveryRuntime(state, old, m); err != nil || selected != dir {
		t.Fatal("did not retain recorded runtime", selected, err)
	}
	if runtime.GOOS != "darwin" {
		if err := prepareUpdate(state, next, old, updateHooks{}); err != nil {
			t.Fatal(err)
		}
		j, _ := readUpdate(state)
		if !fileHashMatches(filepath.Join(updateRoot(state, j), "original/qemu", name), sha256Text(data)) {
			t.Fatal("recovery retained stale sibling runtime")
		}
	}
	mustSave(t, filepath.Join(state, "runtime-selection.json"), selectedRuntime{"unrelated-appliance", dir})
	if _, err := recoveryRuntime(state, old, m); err == nil {
		t.Fatal("mismatched runtime receipt silently ignored")
	}
}

func TestDamagedCandidateDoesNotInvalidateOriginalRecovery(t *testing.T) {
	state, old, next := updateFixture(t)
	if err := prepareUpdate(state, next, old, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	j, err := readUpdate(state)
	if err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(updateRoot(state, j), "candidate", "system.img")
	if err := os.WriteFile(damaged, []byte("damaged candidate evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := publishUpdate(state, updateHooks{}); err == nil {
		t.Fatal("activated corrupt candidate")
	}
	requireUnchanged(t, state, old)
	dest := filepath.Join(t.TempDir(), "recovered")
	if err := recoverUpdate(state, dest, updateHooks{}); err != nil {
		t.Fatal("candidate corruption blocked original recovery", err)
	}
	requireUnchanged(t, dest, old)
	b, _ := os.ReadFile(damaged)
	if string(b) != "damaged candidate evidence" {
		t.Fatal("failed candidate evidence removed")
	}
}

func TestUpdatePreparationResumesPinnedDownload(t *testing.T) {
	state, old, next := updateFixture(t)
	m, _ := readManifest(filepath.Join(next, "manifest.json"))
	archive := bundleArchive(t, m)
	cut := len(archive) / 2
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(archive)))
			w.Write(archive[:cut])
			return
		}
		if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", cut) {
			t.Error("preparation did not resume its partial pinned archive")
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(archive)-cut))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", cut, len(archive)-1, len(archive)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(archive[cut:])
	}))
	defer server.Close()
	url, digest, client := applianceURL, applianceSHA256, newDownloadClient
	applianceURL, applianceSHA256, newDownloadClient = server.URL, sha256Text(archive), server.Client
	t.Cleanup(func() { applianceURL, applianceSHA256, newDownloadClient = url, digest, client })
	if err := prepareUpdate(state, "", old, updateHooks{}); err == nil {
		t.Fatal("truncated download was accepted")
	}
	requireUnchanged(t, state, old)
	if err := prepareUpdate(state, "", old, updateHooks{}); err != nil {
		t.Fatal("pinned download did not resume", err)
	}
	if requests != 2 {
		t.Fatal("unexpected requests", requests)
	}
	requireUnchanged(t, state, old)
}

type updateRoundTripper func(*http.Request) (*http.Response, error)

func (f updateRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateCommitRequiresHealthAndWorkloadAcceptance(t *testing.T) {
	state, old, next := updateFixture(t)
	if err := prepareUpdate(state, next, old, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := publishUpdate(state, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                    string
		ack, healthy, k3s, kite bool
		want                    bool
	}{
		{"no-attestation", false, true, true, true, false},
		{"launcher-only", true, true, false, false, false},
		{"unhealthy", true, false, true, true, false},
		{"validated", true, true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mustJSON := map[string]any{"healthy": tc.healthy, "k3s": tc.k3s, "kite": tc.kite, "protocol": 1, "capabilities": protocol.Capabilities()}
			if err := json.NewEncoder(rec).Encode(mustJSON); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: updateRoundTripper(func(*http.Request) (*http.Response, error) { return rec.Result(), nil })}
			body := `{"workloads_validated":false}`
			if tc.ack {
				body = `{"workloads_validated":true}`
			}
			err := commitUpdateWithHooks(state, client, httptest.NewRequest("POST", "/", strings.NewReader(body)), updateHooks{boundary: func(s string) error {
				if s == "committed" && tc.want {
					return errors.New("crash after commit publication")
				}
				return nil
			}})
			if tc.want && err != nil && err.Error() == "crash after commit publication" {
				err = checkUpdateStart(state)
			}
			if (err == nil) != tc.want {
				t.Fatal(err)
			}
		})
	}
	if err := checkUpdateStart(state); err != nil {
		t.Fatal(err)
	}
	m, _ := readManifest(filepath.Join(state, "release.json"))
	dir, err := retainedUpdateBundle(state, m)
	if err != nil || dir == "" {
		t.Fatal("committed candidate unavailable", err)
	}
	j, _ := readUpdate(state)
	if j.ValidatedAt == "" {
		t.Fatal("missing validation evidence")
	}
	if _, err := verifyUpdatePoint(state, j); err != nil {
		t.Fatal("commit removed recovery point", err)
	}
	// A second preparation must not lose the previous active generation when
	// cancelled, and must never delete the last viable recovery point.
	third, n := testBundle(t)
	n.Version = "0.1.2"
	mustSave(t, filepath.Join(third, "manifest.json"), n)
	if err := prepareUpdate(state, third, dir, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	if err := cancelUpdate(state, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyUpdatePoint(state, j); err != nil {
		t.Fatal(err)
	}
	found, err := retainedUpdateBundle(state, m)
	if err != nil || found != dir {
		t.Fatal("cancelled next update lost active generation", found, err)
	}
}

func TestRecoveryEveryDurableExportBoundary(t *testing.T) {
	state, old, next := updateFixture(t)
	if err := prepareUpdate(state, next, old, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	var boundaries []string
	if err := recoverUpdate(state, filepath.Join(t.TempDir(), "complete"), updateHooks{boundary: func(s string) error {
		boundaries = append(boundaries, s)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			// Export does not mutate the source disk or recovery point.
			dest := filepath.Join(t.TempDir(), "partial")
			err := recoverUpdate(state, dest, updateHooks{boundary: func(s string) error {
				if s == boundary {
					return errors.New("interrupted")
				}
				return nil
			}})
			if err == nil {
				t.Fatal("missing interruption")
			}
			requireUnchanged(t, state, old)
			if boundary != "recovery-exported-ready" && (checkUpdateStart(dest) == nil || cancelUpdate(dest, updateHooks{}) == nil) {
				t.Fatal("partial export was bootable")
			}
			if boundary == "recovery-exported-ready" {
				requireUnchanged(t, dest, old)
			}
			entries, err := os.ReadDir(dest)
			if err != nil || len(entries) == 0 {
				t.Fatal("partial evidence was removed")
			}
		})
	}
}

func TestUpdateJournalCorruptionFailsClosed(t *testing.T) {
	for _, body := range []string{`broken`, `{"format":1,"id":"../../escape","phase":"committed"}`, `{"format":1,"id":"transaction-1","phase":"future"}`} {
		state := t.TempDir()
		os.WriteFile(filepath.Join(state, "update.json"), []byte(body), 0600)
		if checkUpdateStart(state) == nil {
			t.Fatal("corrupt journal accepted")
		}
	}
}

func TestRecoveryDoesNotOverwriteDestination(t *testing.T) {
	state, old, next := updateFixture(t)
	if err := prepareUpdate(state, next, old, updateHooks{}); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	before := []byte("do not overwrite")
	os.WriteFile(filepath.Join(dest, "data.img"), before, 0600)
	if recoverUpdate(state, dest, updateHooks{}) == nil {
		t.Fatal("accepted existing destination")
	}
	after, _ := os.ReadFile(filepath.Join(dest, "data.img"))
	if !bytes.Equal(before, after) {
		t.Fatal("overwrote destination")
	}
}
