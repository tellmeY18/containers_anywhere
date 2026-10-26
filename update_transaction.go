package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
	"github.com/tellmeY18/containers_anywhere/internal/protocol"
)

// The journal is the launch gate, not a second selected-appliance pointer.
// release.json is only changed behind this gate, before any candidate executes.
type updateJournal struct {
	Format      int      `json:"format"`
	ID          string   `json:"id"`
	Phase       string   `json:"phase"`
	Original    manifest `json:"original"`
	Candidate   manifest `json:"candidate"`
	Recovery    string   `json:"recovery_destination,omitempty"`
	ValidatedAt string   `json:"validated_at,omitempty"`
}

type recoveryFile struct {
	SHA256 string      `json:"sha256"`
	Mode   os.FileMode `json:"mode"`
}

type updatePoint struct {
	Files map[string]recoveryFile `json:"files"`
}

type updateHooks struct {
	// Tests interrupt after each durable boundary, including metadata publication.
	boundary func(string) error
	free     func(string) (uint64, error)
}

func (h updateHooks) reached(name string) error {
	if h.boundary != nil {
		return h.boundary(name)
	}
	return nil
}

func updateRoot(state string, j updateJournal) string {
	return filepath.Join(state, "updates", j.ID)
}

func readUpdate(state string) (updateJournal, error) {
	var j updateJournal
	b, err := os.ReadFile(filepath.Join(state, "update.json"))
	if err != nil {
		return j, err
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, fmt.Errorf("invalid update journal; preserve state for operator recovery: %w", err)
	}
	if j.Format != 1 || !strings.HasPrefix(j.ID, "transaction-") || filepath.Base(j.ID) != j.ID || strings.ContainsAny(j.ID, "/\\:") {
		return j, errors.New("invalid update journal identity; preserve state for operator recovery")
	}
	switch j.Phase {
	case "preparing", "prepared", "activating", "candidate-running", "committed", "cancelled", "recovery-exported", "exporting":
	default:
		return j, errors.New("unknown update phase; preserve state for operator recovery")
	}
	return j, nil
}

func writeUpdate(state string, j *updateJournal, phase string, h updateHooks) error {
	j.Phase = phase
	if err := saveJSON(filepath.Join(state, "update.json"), j); err != nil {
		return err
	}
	return h.reached(phase)
}

func checkUpdateStart(state string) error {
	j, err := readUpdate(state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Phase == "cancelled" || j.Phase == "committed" {
		want := j.Original
		if j.Phase == "committed" {
			want = j.Candidate
		}
		selected, err := readManifest(filepath.Join(state, "release.json"))
		if err != nil || !compatibleData(selected, want) {
			return errors.New("update journal and selected identity disagree; preserve current disk for operator recovery")
		}
		return nil
	}
	return fmt.Errorf("update %s is %s; ordinary startup is blocked; use update --operation status, explicitly activate/retry, cancel before execution, or recover to a new directory; data has not been rewound", j.ID, j.Phase)
}

func updateDataCompatible(old, next manifest) error {
	if err := validateLayers(old); err != nil {
		return err
	}
	if err := validateLayers(next); err != nil {
		return err
	}
	if old.Arch != runtime.GOARCH || next.Arch != old.Arch ||
		old.DataContract != "var-lib-v1" || next.DataContract != old.DataContract ||
		old.K3s == "" || old.K3s != next.K3s || old.Kite == "" || old.Kite != next.Kite ||
		old.DataBytes != next.DataBytes {
		return errors.New("unsupported data contract: this update requires var-lib-v1, identical K3s/Kite schema versions, architecture and disk geometry; reset required for unsupported changes: use a new empty state directory, never delete existing data")
	}
	return nil
}

func updateLock(state string, stop bool) (func(), error) {
	unlock, err := lockState(state)
	if err == nil || !stop {
		return unlock, err
	}
	c, controlErr := readControl(state)
	if controlErr != nil {
		return nil, fmt.Errorf("cannot acquire update lock; stop the cluster and wait for other operations: %w", err)
	}
	if _, err = controlCall(c, "POST", "/control/stop", nil, 160*time.Second); err != nil {
		return nil, fmt.Errorf("graceful stop failed; update not started: %w", err)
	}
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		if unlock, err = lockState(state); err == nil {
			return unlock, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("cluster has not released its lifecycle lock: %w", err)
}

func lockUpdateGateway(state string) (func(), error) {
	dir := filepath.Join(state, "gateway-lock")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return func() {}, nil
	} else if err != nil {
		return nil, err
	}
	return lockState(dir)
}

func runUpdate(state, operation, candidate, original, destination string, port int, noOpen, effects, validated, loss bool) error {
	switch operation {
	case "status":
		j, err := readUpdate(state)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(j)
	case "activate":
		if !effects {
			return errors.New("activation can execute cron, webhooks and external writes immediately; no isolation or automatic rollback; require --accept-external-effects")
		}
		return serveUpdate(state, "", port, noOpen, true)
	case "commit":
		if !validated {
			return errors.New("validate workload/PVC contents and credentials through the candidate first; require --accept-workload-validation (launcher health alone is insufficient)")
		}
		c, err := readControl(state)
		if err != nil {
			return err
		}
		b, err := controlCall(c, "POST", "/control/update/commit", []byte(`{"workloads_validated":true}`), 30*time.Second)
		if err == nil {
			_, err = os.Stdout.Write(b)
		}
		return err
	case "prepare", "cancel", "recover":
	default:
		return errors.New("update operation must be prepare, activate, commit, cancel, recover or status")
	}
	unlock, err := updateLock(state, operation == "prepare")
	if err != nil {
		return err
	}
	defer unlock()
	unlockGateway, err := lockUpdateGateway(state)
	if err != nil {
		return err
	}
	defer unlockGateway()
	switch operation {
	case "prepare":
		return prepareUpdate(state, candidate, original, updateHooks{})
	case "cancel":
		return cancelUpdate(state, updateHooks{})
	default:
		if !loss {
			return errors.New("recovery discards later writes in the exported copy and cannot undo external effects; reconcile/fence external systems and require --accept-data-loss; current disk is retained")
		}
		return recoverUpdate(state, destination, updateHooks{})
	}
}

func prepareUpdate(state, candidate, original string, h updateHooks) error {
	var resume *updateJournal
	if old, err := readUpdate(state); err == nil {
		if old.Phase == "preparing" {
			resume = &old
		} else if old.Phase != "cancelled" && old.Phase != "committed" {
			return errors.New("an update is already pending; inspect, cancel before execution, or recover explicitly")
		} else if err := saveJSON(filepath.Join(updateRoot(state, old), "result.json"), old); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	selected, err := readManifest(filepath.Join(state, "release.json"))
	if err != nil {
		return err
	}
	if original == "" {
		original, _ = cachedBundle(filepath.Join(state, "appliances"), selected)
		if original == "" {
			original, err = retainedUpdateBundle(state, selected)
			if err != nil {
				return err
			}
		}
	}
	old, err := verifyBundle(original)
	if err != nil || !compatibleData(selected, old) {
		return errors.New("exact selected appliance is required for recovery; supply --original-bundle pointing to its verified bundle")
	}
	if err := os.MkdirAll(filepath.Join(state, "updates"), 0700); err != nil {
		return err
	}
	if err := atomicfile.SyncDir(state); err != nil {
		return err
	}
	var root string
	j := updateJournal{Format: 1, Original: old}
	if resume != nil {
		j = *resume
		if !compatibleData(j.Original, old) {
			return errors.New("original identity changed during preparation; operator recovery required")
		}
		root = updateRoot(state, j)
	} else {
		root, err = os.MkdirTemp(filepath.Join(state, "updates"), "transaction-")
		if err != nil {
			return err
		}
		j.ID = filepath.Base(root)
		if err := atomicfile.SyncDir(filepath.Dir(root)); err != nil {
			return err
		}
	}
	if err := writeUpdate(state, &j, "preparing", h); err != nil {
		return err
	}
	// Never call bootstrap here: it intentionally returns the selected appliance,
	// not the app's recommendation. Downloads cannot change selection metadata.
	if candidate == "" {
		candidate = filepath.Join(root, "download", "bundle")
		if err := os.MkdirAll(filepath.Dir(candidate), 0700); err != nil {
			return err
		}
		if err := installArchive(newDownloadClient(), applianceURL, applianceSHA256, candidate, func(s string) { fmt.Println(s) }); err != nil {
			return err
		}
	}
	next, err := verifyBundle(candidate)
	if err != nil {
		return err
	}
	if err := updateDataCompatible(old, next); err != nil {
		return err
	}
	if bundleID(old) == bundleID(next) {
		return errors.New("candidate is already selected; cancel this preparation to continue using it")
	}
	if j.Candidate.Version != "" && !compatibleData(j.Candidate, next) {
		return errors.New("candidate changed during preparation; cancel before preparing a different candidate")
	}
	j.Candidate = next
	if err := writeUpdate(state, &j, "preparing", h); err != nil {
		return err
	}
	sources := map[string]string{}
	names, err := backupMembers(state)
	if err != nil {
		return err
	}
	for _, name := range names {
		sources["state/"+name] = filepath.Join(state, filepath.FromSlash(name))
	}
	for _, name := range []string{"manifest.json", "kernel", "initrd", "system.img", "data.img"} {
		sources["original/bundle/"+name] = filepath.Join(original, name)
		sources["candidate/"+name] = filepath.Join(candidate, name)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	sources["original/"+recoveryLauncherName()] = exe
	if caddySHA256 != "" {
		name := "caddy"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		helper := filepath.Join(filepath.Dir(exe), name)
		if !fileHashMatches(helper, caddySHA256) {
			return errors.New("cannot retain verified Caddy helper; use a complete packaged launcher")
		}
		sources["original/"+name] = helper
	}
	if runtime.GOOS != "darwin" {
		qemu := filepath.Join(filepath.Dir(original), "qemu")
		if _, err := os.Stat(qemu); os.IsNotExist(err) {
			qemu = filepath.Join(filepath.Dir(original), "original", "qemu")
			if _, err := os.Stat(qemu); os.IsNotExist(err) {
				qemu = filepath.Join(runtimeCache(state), "qemu")
			}
		}
		if err := addUpdateTree(sources, "original/qemu", qemu); err != nil {
			return fmt.Errorf("retain the current verified runtime beside the original bundle: %w", err)
		}
		if err := verifyRuntime(qemu); err != nil {
			return err
		}
	}
	if err := updateCapacity(root, sources, h); err != nil {
		return err
	}
	point := updatePoint{Files: map[string]recoveryFile{}}
	for _, name := range sortedUpdateNames(sources) {
		dst := filepath.Join(root, filepath.FromSlash(name))
		if _, err := os.Lstat(dst); err == nil {
			digest, err := hashUpdateFile(sources[name])
			if err != nil || !fileHashMatches(dst, digest) {
				return errors.New("partial or changed retained file; cancel and prepare again without deleting evidence: " + name)
			}
		} else {
			if !os.IsNotExist(err) {
				return err
			}
			if err := retainUpdateFile(sources[name], dst); err != nil {
				return err
			}
		}
		digest, err := hashUpdateFile(dst)
		if err != nil {
			return err
		}
		info, err := os.Stat(dst)
		if err != nil {
			return err
		}
		point.Files[name] = recoveryFile{SHA256: digest, Mode: info.Mode().Perm()}
		if err := h.reached("retained:" + name); err != nil {
			return err
		}
	}
	if m, err := verifyBundle(filepath.Join(root, "candidate")); err != nil || !compatibleData(m, next) {
		return errors.New("candidate changed while retaining artifacts")
	}
	if m, err := verifyBundle(filepath.Join(root, "original", "bundle")); err != nil || !compatibleData(m, old) {
		return errors.New("original changed while retaining artifacts")
	}
	if err := syncUpdateDirectories(root); err != nil {
		return err
	}
	if err := saveJSON(filepath.Join(root, "recovery.json"), point); err != nil {
		return err
	}
	if err := h.reached("recovery-point"); err != nil {
		return err
	}
	if err := writeUpdate(state, &j, "prepared", h); err != nil {
		return err
	}
	fmt.Println("Update prepared. Cluster is stopped; original selection is unchanged. Local cold recovery point:", root)
	fmt.Println("This is not off-device protection. Activation requires explicit acceptance of external side effects; automatic rollback is disabled.")
	return nil
}

func sortedUpdateNames[T any](files map[string]T) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func addUpdateTree(sources map[string]string, prefix, root string) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("update recovery rejects links and non-regular files")
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sources[prefix+"/"+filepath.ToSlash(rel)] = p
		return nil
	})
}

func updateCapacity(root string, sources map[string]string, h updateHooks) error {
	var needed uint64 = 256 << 20
	for _, src := range sources {
		info, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return errors.New("update recovery requires regular files")
		}
		if ^uint64(0)-needed < uint64(info.Size()) {
			return errors.New("update recovery size overflow")
		}
		needed += uint64(info.Size())
	}
	free := h.free
	if free == nil {
		free = func(p string) (uint64, error) { s, err := diskspace.Of(p); return s.Free, err }
	}
	available, err := free(root)
	if err != nil {
		return err
	}
	if available < needed {
		return fmt.Errorf("insufficient storage for cold recovery and immutable artifacts: need %d bytes, have %d; no selection or data changed", needed, available)
	}
	return nil
}

func retainUpdateFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("recovery rejects links and non-regular files")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	if err := copyExclusive(src, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0600|info.Mode().Perm()&0100); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(dst))
}

func syncUpdateDirectories(root string) error {
	var dirs []string
	if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			dirs = append(dirs, p)
		}
		return err
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := atomicfile.SyncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func hashUpdateFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func recoveryLauncherName() string {
	if runtime.GOOS == "windows" {
		return "launcher.exe"
	}
	return "launcher"
}

func verifyUpdatePoint(state string, j updateJournal) (updatePoint, error) {
	var point updatePoint
	root := updateRoot(state, j)
	raw, err := os.ReadFile(filepath.Join(root, "recovery.json"))
	if err != nil {
		return point, err
	}
	if err := json.Unmarshal(raw, &point); err != nil {
		return point, err
	}
	for _, required := range []string{"state/data.img", "state/release.json", "original/" + recoveryLauncherName(), "original/bundle/manifest.json", "candidate/manifest.json"} {
		if _, ok := point.Files[required]; !ok {
			return point, errors.New("incomplete update recovery point")
		}
	}
	for name, file := range point.Files {
		if !validUpdatePath(name) || !fileHashMatches(filepath.Join(root, filepath.FromSlash(name)), file.SHA256) {
			return point, fmt.Errorf("update recovery integrity failure: %s; preserve all copies", name)
		}
		old, err := verifyBundle(filepath.Join(root, "original", "bundle"))
		if err != nil || !compatibleData(old, j.Original) {
			return point, errors.New("recovery appliance does not match original identity")
		}
		selected, err := readManifest(filepath.Join(root, "state", "release.json"))
		if err != nil || !compatibleData(selected, j.Original) {
			return point, errors.New("recovery disk identity does not match original appliance")
		}
		if runtime.GOOS != "darwin" {
			if err := verifyRuntime(filepath.Join(root, "original", "qemu")); err != nil {
				return point, err
			}
		}
	}
	return point, nil
}

func validUpdatePath(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\:") && filepath.IsLocal(name) &&
		filepath.ToSlash(filepath.Clean(name)) == name
}

func activateUpdate(state string) (string, error) {
	return publishUpdate(state, updateHooks{})
}

func publishUpdate(state string, h updateHooks) (string, error) {
	j, err := readUpdate(state)
	if err != nil {
		return "", err
	}
	switch j.Phase {
	case "prepared", "activating", "candidate-running":
	default:
		return "", fmt.Errorf("cannot activate update in phase %s", j.Phase)
	}
	if j.Phase == "candidate-running" {
		if err := retainUpdateAttempt(state, j); err != nil {
			return "", err
		}
	}
	if err := updateDataCompatible(j.Original, j.Candidate); err != nil {
		return "", err
	}
	if _, err := verifyUpdatePoint(state, j); err != nil {
		return "", err
	}
	selected, err := readManifest(filepath.Join(state, "release.json"))
	if err != nil {
		return "", err
	}
	if !compatibleData(selected, j.Original) && !compatibleData(selected, j.Candidate) {
		return "", errors.New("selected identity differs from both update generations; operator recovery required")
	}
	dir := filepath.Join(updateRoot(state, j), "candidate")
	m, err := verifyBundle(dir)
	if err != nil || !compatibleData(j.Candidate, m) {
		return "", errors.New("candidate identity mismatch; preserve update evidence")
	}
	if err := writeUpdate(state, &j, "activating", h); err != nil {
		return "", err
	}
	if err := saveJSON(filepath.Join(state, "release.json"), j.Candidate); err != nil {
		return "", err
	}
	if err := h.reached("selection-published"); err != nil {
		return "", err
	}
	// The retained candidate is the source of truth; an old download receipt must
	// never redownload an unrelated generation after publication.
	if err := os.Remove(filepath.Join(state, "appliance.json")); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := atomicfile.SyncDir(state); err != nil {
		return "", err
	}
	if err := h.reached("receipt-removed"); err != nil {
		return "", err
	}
	// Written BEFORE boot: even if the process dies during bootVM, assume external
	// effects occurred. Retry uses the current disk, never the cold recovery copy.
	if err := writeUpdate(state, &j, "candidate-running", h); err != nil {
		return "", err
	}
	return dir, nil
}

func retainUpdateAttempt(state string, j updateJournal) error {
	dir, err := os.MkdirTemp(updateRoot(state, j), "attempt-")
	if err != nil {
		return err
	}
	for _, name := range []string{"console.log", "gateway.log", "launcher.log"} {
		src := filepath.Join(state, name)
		if _, err := os.Lstat(src); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := retainUpdateFile(src, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	if err := saveJSON(filepath.Join(dir, "journal.json"), j); err != nil {
		return err
	}
	return atomicfile.SyncDir(updateRoot(state, j))
}

func retainedUpdateBundle(state string, selected manifest) (string, error) {
	entries, err := os.ReadDir(filepath.Join(state, "updates"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(state, "updates", entry.Name(), "candidate")
		m, err := readManifest(filepath.Join(dir, "manifest.json"))
		if err != nil || !compatibleData(m, selected) {
			continue
		}
		if _, err := verifyBundle(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	return "", nil
}

var updateCommitMu sync.Mutex

func commitUpdate(state string, client *http.Client, r *http.Request) error {
	return commitUpdateWithHooks(state, client, r, updateHooks{})
}

func commitUpdateWithHooks(state string, client *http.Client, r *http.Request, h updateHooks) error {
	updateCommitMu.Lock()
	defer updateCommitMu.Unlock()
	var acceptance struct {
		Workloads bool `json:"workloads_validated"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&acceptance); err != nil || !acceptance.Workloads {
		return errors.New("explicit workload/PVC and credential validation is required")
	}
	j, err := readUpdate(state)
	if err != nil {
		return err
	}
	if j.Phase != "candidate-running" {
		return errors.New("only a running, uncommitted candidate can be committed")
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", "http://guest/_agent/status", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var health struct {
		Healthy, K3s, Kite bool
		Protocol           int
		Capabilities       []string
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health) != nil ||
		!health.Healthy || !health.K3s || !health.Kite || !protocol.Supports(health.Protocol, health.Capabilities) {
		return errors.New("candidate VM/K3s/Kite or protocol is not healthy; not committed")
	}
	j.ValidatedAt = time.Now().UTC().Format(time.RFC3339)
	return writeUpdate(state, &j, "committed", h)
}

func cancelUpdate(state string, h updateHooks) error {
	j, err := readUpdate(state)
	if err != nil {
		return err
	}
	if j.Phase != "preparing" && j.Phase != "prepared" {
		return errors.New("candidate may have executed; cancellation cannot rewind data; retry activation or explicitly recover to a new directory")
	}
	if err := writeUpdate(state, &j, "cancelled", h); err != nil {
		return err
	}
	fmt.Println("Preparation cancelled; original selection and disk unchanged. All update evidence retained.")
	return nil
}

func recoverUpdate(state, destination string, h updateHooks) error {
	if destination == "" {
		return errors.New("--destination must name a new, empty directory outside cluster state")
	}
	dest, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil {
		return err
	}
	dest = filepath.Join(parent, filepath.Base(dest))
	source, err := filepath.EvalSymlinks(state)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(source, dest)
	if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("recovery destination must be outside cluster state")
	}
	j, err := readUpdate(state)
	if err != nil {
		return err
	}
	if j.Phase == "preparing" || j.Phase == "cancelled" {
		return errors.New("no complete recovery point; original data remains selected")
	}
	point, err := verifyUpdatePoint(state, j)
	if err != nil {
		return err
	}
	// Require nonexistence, stricter than empty, so two recoveries cannot adopt
	// each other's partial output. Partial exports are intentionally retained.
	if err := os.Mkdir(dest, 0700); err != nil {
		return fmt.Errorf("recovery requires a new destination; preserve partial exports: %w", err)
	}
	if err := atomicfile.SyncDir(filepath.Dir(dest)); err != nil {
		return err
	}
	unlock, err := lockState(dest)
	if err != nil {
		return err
	}
	defer unlock()
	blocked := updateJournal{Format: 1, ID: j.ID, Original: j.Original, Phase: "exporting"}
	if err := saveJSON(filepath.Join(dest, "update.json"), blocked); err != nil {
		return err
	}
	sources := map[string]string{}
	for name := range point.Files {
		switch {
		case strings.HasPrefix(name, "state/"):
			sources[strings.TrimPrefix(name, "state/")] = filepath.Join(updateRoot(state, j), filepath.FromSlash(name))
		case strings.HasPrefix(name, "original/"):
			sources["recovery/"+strings.TrimPrefix(name, "original/")] = filepath.Join(updateRoot(state, j), filepath.FromSlash(name))
		}
	}
	if err := updateCapacity(dest, sources, h); err != nil {
		return err
	}
	for _, name := range sortedUpdateNames(sources) {
		if err := retainUpdateFile(sources[name], filepath.Join(dest, filepath.FromSlash(name))); err != nil {
			return err
		}
		if err := syncUpdateDirectories(dest); err != nil {
			return err
		}
		if err := h.reached("exported:" + name); err != nil {
			return err
		}
	}
	j.Recovery = dest
	if err := writeUpdate(state, &j, "recovery-exported", h); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dest, "update.json")); err != nil {
		return err
	}
	if err := atomicfile.SyncDir(dest); err != nil {
		return err
	}
	fmt.Printf("Cold copy exported to %s; current/failed disk retained at %s. No VM started.\n", dest, state)
	fmt.Printf("Fence the old/current instance and reconcile external effects before using --bundle %s --state %s.\n", filepath.Join(dest, "recovery", "bundle"), dest)
	return h.reached("recovery-exported-ready")
}
