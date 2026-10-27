package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
)

// A root swap replaces only the read-only root image (kernel, initrd, system
// image) and leaves the data disk untouched. It is allowed only when the K3s
// version is identical, so the previous root can always read the data again.
// That is what makes automatic fallback safe and the data copy unnecessary.
// Anything else (a K3s patch release, for example) uses the full update
// transaction in update_transaction.go.
//
// Phases: trying -> committed | rolled-back. "trying" is durable before the
// candidate is selected, so a crash or power loss mid-trial rolls back to the
// original root on the next start.
type swapJournal struct {
	Format    int      `json:"format"`
	ID        string   `json:"id"`
	Phase     string   `json:"phase"`
	Original  manifest `json:"original"`
	Candidate manifest `json:"candidate"`
	Reason    string   `json:"reason,omitempty"`
}

func readSwap(state string) (swapJournal, error) {
	var j swapJournal
	b, err := os.ReadFile(filepath.Join(state, "swap.json"))
	if err != nil {
		return j, err
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, fmt.Errorf("invalid swap journal; preserve state for operator recovery: %w", err)
	}
	if j.Format != 1 || !strings.HasPrefix(j.ID, "swap-") || filepath.Base(j.ID) != j.ID || strings.ContainsAny(j.ID, "/\\:") {
		return j, errors.New("invalid swap journal identity; preserve state for operator recovery")
	}
	switch j.Phase {
	case "trying", "committed", "rolled-back":
	default:
		return j, errors.New("unknown swap phase; preserve state for operator recovery")
	}
	return j, nil
}

func writeSwap(state string, j *swapJournal, phase, reason string, h updateHooks) error {
	j.Phase, j.Reason = phase, reason
	if err := saveJSON(filepath.Join(state, "swap.json"), j); err != nil {
		return err
	}
	return h.reached("swap-" + phase)
}

// The process-local session marker keeps recoverSwap from rolling back the
// trial that this very process is running. A crash clears it by design.
var (
	swapMu      sync.Mutex
	swapSession string
	swapAbort   chan struct{}
)

func swapSessionID() string {
	swapMu.Lock()
	defer swapMu.Unlock()
	return swapSession
}

func newSwapAbort() chan struct{} {
	swapMu.Lock()
	defer swapMu.Unlock()
	swapAbort = make(chan struct{})
	return swapAbort
}

func currentSwapAbort() <-chan struct{} {
	swapMu.Lock()
	defer swapMu.Unlock()
	return swapAbort
}

func abortSwapSession() {
	swapMu.Lock()
	defer swapMu.Unlock()
	if swapAbort != nil {
		close(swapAbort)
		swapAbort = nil
	}
}

func swapEligible(old, next manifest) error {
	if err := updateDataCompatible(old, next); err != nil {
		return err
	}
	if identical, _ := k3sChange(old.K3s, next.K3s); !identical {
		return fmt.Errorf("K3s changes from %s to %s: a root swap needs an identical K3s version; use the full update transaction (prepare, activate, commit)", old.K3s, next.K3s)
	}
	if bundleID(old) == bundleID(next) {
		return errors.New("candidate is already the selected appliance")
	}
	return nil
}

// retainSwapBundle lays the bundle out as <dir>/candidate with the runtime at
// <dir>/original/qemu, the layout the launcher already resolves for retained
// bundles, so either generation can be selected later without any download.
func retainSwapBundle(state, dir, bundle, runtimeDir string, link bool) error {
	for _, name := range []string{"manifest.json", "kernel", "initrd", "system.img", "data.img"} {
		if err := retainSwapFile(filepath.Join(bundle, name), filepath.Join(dir, "candidate", name), link); err != nil {
			return err
		}
	}
	if runtimeDir == "" {
		return nil
	}
	sources := map[string]string{}
	if err := addUpdateTree(sources, "original/qemu", runtimeDir); err != nil {
		return err
	}
	for name, src := range sources {
		if err := retainSwapFile(src, filepath.Join(dir, filepath.FromSlash(name)), true); err != nil {
			return err
		}
	}
	return nil
}

// Bundle files are immutable and hash-verified on every launch, so hard links
// are safe for files that are already under the state directory.
func retainSwapFile(src, dst string, link bool) error {
	if link {
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		if err := os.Link(src, dst); err == nil {
			return atomicfile.SyncDir(filepath.Dir(dst))
		}
	}
	return retainUpdateFile(src, dst)
}

func insideState(state, path string) bool {
	rel, err := filepath.Rel(state, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func prepareSwap(state, candidate, original string, h updateHooks) (swapJournal, error) {
	var j swapJournal
	if old, err := readSwap(state); err == nil && old.Phase == "trying" {
		return j, errors.New("a previous root swap was interrupted; start the cluster normally to roll it back before swapping again")
	} else if err != nil && !os.IsNotExist(err) {
		return j, err
	}
	if u, err := readUpdate(state); err == nil {
		if u.Phase != "committed" && u.Phase != "cancelled" {
			return j, errors.New("a full update is pending; inspect, cancel or recover it before swapping")
		}
	} else if !os.IsNotExist(err) {
		return j, err
	}
	selected, err := readManifest(filepath.Join(state, "release.json"))
	if err != nil {
		return j, err
	}
	if original == "" {
		original, _ = cachedBundle(filepath.Join(state, "appliances"), selected)
		if original == "" {
			if original, err = retainedUpdateBundle(state, selected); err != nil {
				return j, err
			}
		}
	}
	old, err := verifyBundle(original)
	if err != nil || !compatibleData(selected, old) {
		return j, errors.New("exact selected appliance is required for fallback; supply --original-bundle pointing to its verified bundle")
	}
	downloaded := ""
	if candidate == "" {
		downloaded = filepath.Join(state, "updates", "swap-download-"+randomToken()[:12])
		candidate = filepath.Join(downloaded, "bundle")
		if err := os.MkdirAll(downloaded, 0700); err != nil {
			return j, err
		}
		defer os.RemoveAll(downloaded)
		if err := installArchive(newDownloadClient(), applianceURL, applianceSHA256, candidate, func(s string) { fmt.Println(s) }); err != nil {
			return j, err
		}
	}
	next, err := verifyBundle(candidate)
	if err != nil {
		return j, err
	}
	if err := swapEligible(old, next); err != nil {
		return j, err
	}
	runtimeDir, err := swapRuntime(state, original, old)
	if err != nil {
		return j, err
	}
	id := "swap-" + randomToken()[:12]
	sources := map[string]string{}
	for _, name := range []string{"manifest.json", "kernel", "initrd", "system.img", "data.img"} {
		sources["candidate/"+name] = filepath.Join(candidate, name)
		sources["original/"+name] = filepath.Join(original, name)
	}
	if runtimeDir != "" {
		if err := addUpdateTree(sources, "runtime", runtimeDir); err != nil {
			return j, err
		}
	}
	if err := updateCapacity(state, sources, h); err != nil {
		return j, err
	}
	if err := os.MkdirAll(filepath.Join(state, "updates"), 0700); err != nil {
		return j, err
	}
	candidateDir := filepath.Join(state, "updates", id)
	rollbackDir := filepath.Join(state, "updates", id+"-original")
	if err := retainSwapBundle(state, candidateDir, candidate, runtimeDir, insideState(state, candidate)); err != nil {
		return j, err
	}
	if err := retainSwapBundle(state, rollbackDir, original, runtimeDir, insideState(state, original)); err != nil {
		return j, err
	}
	for _, check := range []struct {
		dir  string
		want manifest
	}{{candidateDir, next}, {rollbackDir, old}} {
		if m, err := verifyBundle(filepath.Join(check.dir, "candidate")); err != nil || !compatibleData(m, check.want) {
			return j, errors.New("retained root changed while copying; the original selection is unchanged")
		}
	}
	if err := h.reached("swap-retained"); err != nil {
		return j, err
	}
	// Keep the original download receipt so a rollback can restore it.
	receipt := filepath.Join(state, "appliance.json")
	if b, err := os.ReadFile(receipt); err == nil {
		if err := atomicfile.WritePrivate(filepath.Join(rollbackDir, "appliance.json"), b); err != nil {
			return j, err
		}
	}
	j = swapJournal{Format: 1, ID: id, Original: old, Candidate: next}
	if err := writeSwap(state, &j, "trying", "", h); err != nil {
		return j, err
	}
	// A finished full update leaves a journal that names the old identity as
	// selected; archive it so the new selection is not reported as a mismatch.
	if u, err := readUpdate(state); err == nil {
		if err := saveJSON(filepath.Join(updateRoot(state, u), "result.json"), u); err != nil {
			return j, err
		}
		if err := os.Remove(filepath.Join(state, "update.json")); err != nil {
			return j, err
		}
	}
	if err := saveJSON(filepath.Join(state, "release.json"), next); err != nil {
		return j, err
	}
	if err := h.reached("swap-selected"); err != nil {
		return j, err
	}
	if err := os.Remove(receipt); err != nil && !os.IsNotExist(err) {
		return j, err
	}
	return j, atomicfile.SyncDir(state)
}

func swapRuntime(state, original string, old manifest) (string, error) {
	if runtime.GOOS == "darwin" {
		return "", nil
	}
	return recoveryRuntime(state, original, old)
}

// rollbackSwap reselects the original root. The data disk is not rewound: the
// original root can read it because K3s and the data contract are unchanged.
func rollbackSwap(state string, j *swapJournal, reason string, h updateHooks) error {
	selected, err := readManifest(filepath.Join(state, "release.json"))
	if err != nil {
		return err
	}
	if !compatibleData(selected, j.Original) && !compatibleData(selected, j.Candidate) {
		return errors.New("selected identity differs from both swap generations; operator recovery required")
	}
	if err := saveJSON(filepath.Join(state, "release.json"), j.Original); err != nil {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(state, "updates", j.ID+"-original", "appliance.json")); err == nil {
		if err := atomicfile.WritePrivate(filepath.Join(state, "appliance.json"), b); err != nil {
			return err
		}
	}
	if err := atomicfile.SyncDir(state); err != nil {
		return err
	}
	return writeSwap(state, j, "rolled-back", reason, h)
}

// recoverSwap runs under the lifecycle lock at every start. A trial that never
// reached a verdict (crash, power loss, killed launcher) is rolled back.
func recoverSwap(state string, h updateHooks) error {
	j, err := readSwap(state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Phase != "trying" || swapSessionID() == j.ID {
		return nil
	}
	fmt.Println("A previous root swap never reached a verdict; rolling back to the original root. The data disk is unchanged.")
	return rollbackSwap(state, &j, "interrupted before a verdict", h)
}

type swapHooks struct {
	serve   func() error // runs one serving session with the currently selected root
	abort   func()       // makes a running session shut down
	healthy func() bool
	timeout time.Duration
	poll    time.Duration
	update  updateHooks
}

// runSwapSession tries the candidate. Healthy within the timeout commits it.
// A failed, exited or unhealthy trial rolls back and serves the original root
// in the same session. Quitting during the trial leaves it durable as "trying",
// which the next start rolls back.
func runSwapSession(state string, j *swapJournal, h swapHooks) error {
	done := make(chan error, 1)
	go func() { done <- h.serve() }()
	deadline := time.NewTimer(h.timeout)
	defer deadline.Stop()
	tick := time.NewTicker(h.poll)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err == nil {
				return nil
			}
			return swapFallback(state, j, "candidate session failed: "+err.Error(), h)
		case <-tick.C:
			if h.healthy() {
				if err := writeSwap(state, j, "committed", "", h.update); err != nil {
					h.abort()
					<-done
					return err
				}
				fmt.Println("Root swap committed: the candidate root is healthy. The previous root is retained for fallback.")
				return <-done
			}
		case <-deadline.C:
			h.abort()
			<-done
			return swapFallback(state, j, fmt.Sprintf("candidate was not healthy within %s", h.timeout), h)
		}
	}
}

func swapFallback(state string, j *swapJournal, reason string, h swapHooks) error {
	fmt.Println("Root swap failed; rolling back to the original root:", reason)
	if err := rollbackSwap(state, j, reason, h.update); err != nil {
		return fmt.Errorf("%s; rollback also failed: %w", reason, err)
	}
	return h.serve()
}

func swapTimeout() time.Duration {
	if s := os.Getenv("CONTAINERS_SWAP_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 10 * time.Minute
}

func runSwap(state, candidate, original string, port int, noOpen bool) error {
	unlock, err := updateLock(state, true)
	if err != nil {
		return err
	}
	unlockGateway, err := lockUpdateGateway(state)
	if err != nil {
		unlock()
		return err
	}
	j, err := prepareSwap(state, candidate, original, updateHooks{})
	unlockGateway()
	unlock()
	if err != nil {
		return err
	}
	fmt.Printf("Trying root %s with the existing data disk (K3s %s unchanged). It falls back to %s automatically if it is not healthy.\n",
		j.Candidate.Version, j.Candidate.K3s, j.Original.Version)
	swapMu.Lock()
	swapSession = j.ID
	swapMu.Unlock()
	defer func() {
		swapMu.Lock()
		swapSession = ""
		swapMu.Unlock()
	}()
	return runSwapSession(state, &j, swapHooks{
		serve: func() error {
			newSwapAbort()
			return serveUpdate(state, "", port, noOpen, false)
		},
		abort:   abortSwapSession,
		healthy: func() bool { return swapHealthy(state) },
		timeout: swapTimeout(),
		poll:    2 * time.Second,
	})
}

func swapHealthy(state string) bool {
	c, err := readControl(state)
	if err != nil {
		return false
	}
	b, err := controlCall(c, "GET", "/control/status", nil, 5*time.Second)
	if err != nil {
		return false
	}
	var s struct{ Healthy, Console bool }
	return json.Unmarshal(b, &s) == nil && s.Healthy && s.Console
}
