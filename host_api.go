package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
	"github.com/tellmeY18/containers_anywhere/internal/hostapi"
	"github.com/tellmeY18/containers_anywhere/internal/protocol"
)

// The host bridge is the launcher's half of the generic host API. It pushes facts
// that only the host knows to the guest agent, lists the requests that workloads
// have made, and applies the ones a person approves in the launcher. Workloads
// never hold a host credential and cannot approve anything themselves.

// hostPower carries an approved power request to the serving session.
var (
	hostPower        = make(chan string, 1)
	restartAfterStop atomic.Bool
)

// hostAPISupported reports whether the selected appliance advertises the bridge.
func hostAPISupported(m manifest) bool { return protocol.Has(m.Capabilities, "host-api-v1") }

func triggerPower(action string) {
	if action == "restart-appliance" {
		restartAfterStop.Store(true)
	}
	select {
	case hostPower <- action:
	default:
	}
}

type hostBridge struct {
	state     string
	m         manifest
	cpus      int
	memoryMiB int
	gw        *gateway
	client    *http.Client // short timeout, for control traffic
	long      *http.Client // for slow guest operations such as image pruning

	mu       sync.Mutex
	requests []hostapi.Actionable
	inflight map[string]bool
	lan      *hostapi.LANCheck
	lastErr  string
	dest     destinationChecker
	started  time.Time
}

func newHostBridge(state string, m manifest, gw *gateway, client *http.Client) *hostBridge {
	cpus, mem, _ := vmResourcesFor(state)
	return &hostBridge{state: state, m: m, cpus: cpus, memoryMiB: mem, gw: gw, client: client,
		long: &http.Client{Transport: client.Transport, Timeout: 4 * time.Minute}, inflight: map[string]bool{}, started: time.Now()}
}

var applianceTagPattern = regexp.MustCompile(`appliance-v([0-9]+\.[0-9]+\.[0-9]+[^/]*)/`)

func hostUpdateFact(state, current string) hostapi.Update {
	u := hostapi.Update{Phase: "idle"}
	if j, err := readUpdate(state); err == nil {
		u.Kind, u.Phase, u.CandidateVersion = "transaction", j.Phase, j.Candidate.Version
		if blocked := checkUpdateStart(state); blocked != nil {
			u.BlockedReason = trimTo(blocked.Error(), 250)
		}
	} else if s, err := readSwap(state); err == nil {
		u.Kind, u.Phase, u.CandidateVersion = "swap", s.Phase, s.Candidate.Version
	}
	if entries, err := os.ReadDir(filepath.Join(state, "updates")); err == nil {
		for _, e := range entries {
			if e.IsDir() && (strings.HasPrefix(e.Name(), "transaction-") || strings.HasPrefix(e.Name(), "swap-")) && len(u.RetainedRecoveryPoints) < 32 {
				u.RetainedRecoveryPoints = append(u.RetainedRecoveryPoints, e.Name())
			}
		}
		sort.Strings(u.RetainedRecoveryPoints)
	}
	if m := applianceTagPattern.FindStringSubmatch(applianceURL); m != nil && m[1] != current {
		u.AvailableVersion = m[1]
	}
	return u
}

func trimTo(s string, n int) string {
	s = strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func privateLANAddresses() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip := n.IP.To4(); ip != nil && !ip.IsLoopback() && ip.IsPrivate() {
				out = append(out, ip.String())
			}
		}
	}
	sort.Strings(out)
	return out
}

// certFingerprint formats a certificate fingerprint the way openssl prints it, so
// an administrator can compare it by eye with what a client shows.
func certFingerprint(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("not a certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", err
	}
	sum := sha256.Sum256(block.Bytes)
	hexed := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(hexed); i += 2 {
		parts = append(parts, hexed[i:i+2])
	}
	return strings.Join(parts, ":"), nil
}

func gatewayFact(state string, gw *gateway, lan *hostapi.LANCheck) hostapi.Gateway {
	if gw == nil {
		return hostapi.Gateway{}
	}
	cfg := gw.config
	g := hostapi.Gateway{Enabled: true, HTTPPort: cfg.HTTPPort, HTTPSPort: cfg.Port, FallbackPorts: cfg.Port == 8443, LANCheck: lan}
	g.BoundAddresses = cfg.Addresses
	if len(g.BoundAddresses) == 0 {
		g.BoundAddresses = []string{"127.0.0.1"}
	}
	host := "127.0.0.1"
	for _, a := range g.BoundAddresses {
		if a == wildcardAddress {
			if lanAddrs := privateLANAddresses(); len(lanAddrs) > 0 {
				host = lanAddrs[0]
			}
			break
		}
		if ip := net.ParseIP(a); ip != nil && !ip.IsLoopback() {
			host = a
			break
		}
	}
	root := filepath.Join(state, "gateway", "caddy", "pki", "authorities", "local", "root.crt")
	if fp, err := certFingerprint(root); err == nil {
		g.RootCAFingerprintSHA256 = fp
		g.RootCAURL = fmt.Sprintf("http://%s/root.crt", net.JoinHostPort(host, fmt.Sprint(cfg.HTTPPort)))
	}
	return g
}

// runLANCheck dials this computer's own LAN addresses on the HTTPS port. It shows
// the gateway accepts connections there, and nothing more: it cannot prove that
// another device, behind a firewall or on isolated Wi-Fi, can reach it.
func runLANCheck(httpsPort int) *hostapi.LANCheck {
	check := &hostapi.LANCheck{CheckedAt: time.Now().UTC().Format(time.RFC3339), Results: []hostapi.LANResult{},
		Note: "This computer connected to its own LAN addresses. It does not prove that other devices can reach it; firewalls, Wi-Fi client isolation and multicast filtering can still block them."}
	for _, a := range privateLANAddresses() {
		r := hostapi.LANResult{Address: a}
		c, err := net.DialTimeout("tcp4", net.JoinHostPort(a, fmt.Sprint(httpsPort)), 2*time.Second)
		if err != nil {
			r.Error = trimTo(err.Error(), 200)
		} else {
			c.Close()
			r.Reachable = true
		}
		check.Results = append(check.Results, r)
		if len(check.Results) >= 16 {
			break
		}
	}
	return check
}

func (b *hostBridge) facts() hostapi.Facts {
	now := time.Now().UTC()
	memTotal, _ := hostMemoryMiB()
	usage, _ := diskspace.Of(b.state)
	cpu := hostapi.Sizing{Allocated: int64(b.cpus), HostTotal: int64(runtime.NumCPU())}
	mem := hostapi.Sizing{Allocated: int64(b.memoryMiB), HostTotal: memTotal}
	if a, ok := readApprovedResources(b.state); ok && (a.CPUs != b.cpus || a.MemoryMiB != b.memoryMiB) {
		c, m := int64(a.CPUs), int64(a.MemoryMiB)
		cpu.Requested, mem.Requested = &c, &m
	}
	as := autostartStatus(b.state)
	b.mu.Lock()
	lan := b.lan
	b.mu.Unlock()
	f := hostapi.Facts{HostTime: now.Format(time.RFC3339), AppVersion: appVersion, ApplianceVersion: b.m.Version, BundleID: bundleID(b.m),
		K3sVersion: b.m.K3s, Arch: b.m.Arch, OS: runtime.GOOS, CPU: cpu, MemoryMiB: mem,
		StateDirFreeBytes: int64(usage.Free), StateDirTotal: int64(usage.Total), LauncherHealthy: true,
		Autostart: hostapi.Autostart{Supported: as.Supported, Enabled: as.Enabled, Mechanism: trimTo(as.Mechanism, 100), Note: trimTo(as.Note, 200)},
		Update:    hostUpdateFact(b.state, b.m.Version), Gateway: gatewayFact(b.state, b.gw, lan),
		Destinations: b.dest.facts(b.state)}
	f.RecoveryPoints, f.BackupSchedule, f.RecoveryKit = recoveryFacts(b.state)
	f.DataDisk = dataDiskFact(b.state)
	return f
}

func (b *hostBridge) guestCall(ctx context.Context, c *http.Client, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://guest"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("guest %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (b *hostBridge) push(ctx context.Context) error {
	body, _ := json.Marshal(b.facts())
	_, err := b.guestCall(ctx, b.client, "PUT", "/_agent/host/facts", body)
	return err
}

func (b *hostBridge) decide(ctx context.Context, d hostapi.Decision) error {
	body, _ := json.Marshal(map[string]any{"decisions": []hostapi.Decision{d}})
	_, err := b.guestCall(ctx, b.client, "POST", "/_agent/host/decisions", body)
	return err
}

func (b *hostBridge) fetch(ctx context.Context) error {
	raw, err := b.guestCall(ctx, b.client, "GET", "/_agent/host/requests", nil)
	if err != nil {
		return err
	}
	var list struct {
		Requests []hostapi.Actionable `json:"requests"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	sort.Slice(list.Requests, func(i, j int) bool { return list.Requests[i].Created < list.Requests[j].Created })
	b.mu.Lock()
	b.requests = list.Requests
	b.mu.Unlock()
	for _, a := range list.Requests {
		switch {
		case a.Phase == hostapi.PhasePending && a.Spec.Type == hostapi.TypeLANCheck:
			// Harmless and read-only, so it needs no approval.
			go b.process(context.Background(), a, true)
		case a.Phase == hostapi.PhaseApproved:
			// Approved before an earlier launcher exited. Power requests are never
			// replayed, so a crash cannot cause a second shutdown.
			go b.process(context.Background(), a, false)
		}
	}
	return nil
}

func (b *hostBridge) claim(uid string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflight[uid] {
		return false
	}
	b.inflight[uid] = true
	return true
}

func (b *hostBridge) release(uid string) {
	b.mu.Lock()
	delete(b.inflight, uid)
	b.mu.Unlock()
}

// checkApproval refuses a request the host cannot or should not satisfy.
func (b *hostBridge) checkApproval(a hostapi.Actionable) error {
	if a.Spec.Type != hostapi.TypeResources {
		return nil
	}
	r := a.Spec.Resources
	if r.DiskGiB > 0 {
		cur := int64(0)
		if info, err := os.Stat(filepath.Join(b.state, "data.img")); err == nil {
			cur = info.Size()
		}
		if err := diskGrowthRefusal(b.state, int64(r.DiskGiB)<<30, cur); err != nil {
			return err
		}
	}
	if err := validResources(r.CPUs, r.MemoryMiB); err != nil {
		return err
	}
	if r.CPUs > runtime.NumCPU() {
		return fmt.Errorf("this computer has %d CPUs; the request would oversubscribe it", runtime.NumCPU())
	}
	total, err := hostMemoryMiB()
	if err != nil || total <= 0 {
		return errors.New("cannot determine this computer's memory, so the request is refused")
	}
	if int64(r.MemoryMiB)+2048 > total {
		return fmt.Errorf("this computer has %d MiB; at least 2048 MiB must stay free for the host", total)
	}
	return nil
}

func (b *hostBridge) process(ctx context.Context, a hostapi.Actionable, approve bool) {
	if !b.claim(a.UID) {
		return
	}
	defer b.release(a.UID)
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	if a.Phase == hostapi.PhaseApproved && (a.Spec.Type == hostapi.TypePower || a.Spec.Type == hostapi.TypeRecoveryPoint) {
		b.report(ctx, hostapi.Decision{UID: a.UID, Phase: hostapi.PhaseFailed, Reason: "Interrupted", Message: "The launcher restarted before this was applied; create a new request"})
		return
	}
	if a.Phase == hostapi.PhasePending && approve {
		if err := b.checkApproval(a); err != nil {
			b.report(ctx, hostapi.Decision{UID: a.UID, Phase: hostapi.PhaseRejected, Reason: "Refused", Message: trimTo(err.Error(), 500)})
			return
		}
		if err := b.decide(ctx, hostapi.Decision{UID: a.UID, Phase: hostapi.PhaseApproved, Reason: "ApprovedInLauncher", Message: "Approved in the launcher"}); err != nil {
			b.setErr(err)
			return
		}
	}
	d, after := b.apply(ctx, a)
	b.report(ctx, d)
	if after != nil && d.Phase == hostapi.PhaseApplied {
		after()
	}
}

func (b *hostBridge) report(ctx context.Context, d hostapi.Decision) {
	if err := b.decide(ctx, d); err != nil {
		b.setErr(err)
	}
}

func (b *hostBridge) setErr(err error) {
	b.mu.Lock()
	b.lastErr = trimTo(err.Error(), 300)
	b.mu.Unlock()
}

func failed(uid, reason string, err error) hostapi.Decision {
	return hostapi.Decision{UID: uid, Phase: hostapi.PhaseFailed, Reason: reason, Message: trimTo(err.Error(), 500)}
}

// apply performs the effect. Every effect is idempotent. The returned function
// runs only after the Applied decision has been recorded.
func (b *hostBridge) apply(ctx context.Context, a hostapi.Actionable) (hostapi.Decision, func()) {
	ok := func(msg string, result any) hostapi.Decision {
		d := hostapi.Decision{UID: a.UID, Phase: hostapi.PhaseApplied, Reason: "Applied", Message: msg}
		if result != nil {
			d.Result, _ = json.Marshal(result)
		}
		return d
	}
	switch a.Spec.Type {
	case hostapi.TypeResources:
		r := a.Spec.Resources
		disk := r.DiskGiB
		if prev, found := readApprovedResources(b.state); found && prev.DiskGiB > disk {
			disk = prev.DiskGiB // the disk never shrinks
		}
		if err := saveJSON(filepath.Join(b.state, approvedResourcesFile), approvedResources{CPUs: r.CPUs, MemoryMiB: r.MemoryMiB, DiskGiB: disk, RequestUID: a.UID, ApprovedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			return failed(a.UID, "WriteFailed", err), nil
		}
		msg := "The new size takes effect the next time the appliance starts."
		if r.DiskGiB > 0 {
			msg += " The data disk grows then, and never shrinks."
		}
		return ok(msg, map[string]int{"cpus": r.CPUs, "memoryMiB": r.MemoryMiB, "diskGiB": disk}), nil
	case hostapi.TypePower:
		action := a.Spec.Power.Action
		return ok("The appliance will "+action+" now", map[string]string{"action": action}), func() { triggerPower(action) }
	case hostapi.TypeAutostart:
		if err := setAutostart(b.state, a.Spec.Autostart.Enabled); err != nil {
			return failed(a.UID, "AutostartFailed", err), nil
		}
		return ok(fmt.Sprintf("Autostart is now %v", a.Spec.Autostart.Enabled), autostartStatus(b.state)), nil
	case hostapi.TypeRecoveryPoint:
		p, err := queueRecovery(b.state, a.Spec.RecoveryPoint.Destination, "request")
		if err != nil {
			return failed(a.UID, "RecoveryPointRefused", err), nil
		}
		return ok("The appliance will stop, write an encrypted recovery point, read it back, and start again. Follow its progress in HostRecoveryPoint "+p.ID+".",
			map[string]string{"recoveryPoint": p.ID}), func() { triggerPower("restart-appliance") }
	case hostapi.TypeBackupSchedule:
		s := *a.Spec.BackupSchedule
		if s.Enabled {
			if _, err := findDestination(b.state, s.Destination); err != nil {
				return failed(a.UID, "UnknownDestination", err), nil
			}
		}
		if err := saveSchedule(b.state, s); err != nil {
			return failed(a.UID, "WriteFailed", err), nil
		}
		if !s.Enabled {
			return ok("The backup schedule is cancelled", map[string]bool{"enabled": false}), nil
		}
		return ok(fmt.Sprintf("Recovery points will be written every %d hours inside the window, keeping the newest %d. Each one stops the appliance for a few minutes.", s.EveryHours, s.KeepLast), s), nil
	case hostapi.TypePruneImages:
		raw, err := b.guestCall(ctx, b.long, "POST", "/_agent/host/prune-images", nil)
		if err != nil {
			return failed(a.UID, "PruneFailed", err), nil
		}
		var result json.RawMessage = raw
		return ok("Unused images were removed; volumes and images in use were not touched", result), nil
	case hostapi.TypeLANCheck:
		port := 443
		if b.gw != nil {
			port = b.gw.config.Port
		}
		check := runLANCheck(port)
		b.mu.Lock()
		b.lan = check
		b.mu.Unlock()
		return ok("Checked this computer's LAN addresses", check), nil
	}
	return failed(a.UID, "Unsupported", fmt.Errorf("unknown request type %q", a.Spec.Type)), nil
}

func (b *hostBridge) pending() []hostapi.Actionable {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []hostapi.Actionable{}
	for _, a := range b.requests {
		if a.Phase == hostapi.PhasePending && !b.inflight[a.UID] {
			out = append(out, a)
		}
	}
	return out
}

// Approve is called only from the launcher's own authenticated surfaces.
func (b *hostBridge) Approve(uid string) error {
	for _, a := range b.pending() {
		if a.UID == uid {
			go b.process(context.Background(), a, true)
			return nil
		}
	}
	return errors.New("no such request is awaiting approval")
}

func (b *hostBridge) Reject(uid string) error {
	for _, a := range b.pending() {
		if a.UID == uid {
			if !b.claim(uid) {
				return errors.New("request is being processed")
			}
			defer b.release(uid)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return b.decide(ctx, hostapi.Decision{UID: uid, Phase: hostapi.PhaseRejected, Reason: "RejectedInLauncher", Message: "Rejected in the launcher"})
		}
	}
	return errors.New("no such request is awaiting approval")
}

// Describe is what the control API and the CLI show: what the host reports, what
// is waiting for a person, and what the guest bridge has actually written.
func (b *hostBridge) Describe(ctx context.Context) ([]byte, error) {
	b.mu.Lock()
	lastErr := b.lastErr
	b.mu.Unlock()
	out := map[string]any{"facts": b.facts(), "pending": b.pending(), "bridgeError": lastErr}
	if raw, err := b.guestCall(ctx, b.client, "GET", "/_agent/host/status", nil); err == nil {
		out["agent"] = json.RawMessage(raw)
	} else {
		out["agentError"] = trimTo(err.Error(), 200)
	}
	return json.Marshal(out)
}

func (b *hostBridge) run(ctx context.Context) {
	for {
		step, cancel := context.WithTimeout(ctx, 25*time.Second)
		if err := b.push(step); err != nil {
			b.setErr(err)
		} else if err := b.fetch(step); err != nil {
			b.setErr(err)
		} else {
			b.mu.Lock()
			b.lastErr = ""
			b.mu.Unlock()
		}
		cancel()
		b.startScheduledRecovery()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// startScheduledRecovery starts a scheduled recovery point when one is due. The person
// consented to the downtime when they approved the schedule; it only runs inside its
// window, never in the first minutes after the cluster starts, and backs off after a failure.
func (b *hostBridge) startScheduledRecovery() {
	if time.Since(b.started) < 10*time.Minute {
		return
	}
	dest, ok := scheduleDue(b.state, time.Now())
	if !ok {
		return
	}
	if _, err := queueRecovery(b.state, dest, "schedule"); err != nil {
		log.Printf("scheduled recovery point not started: %v", err)
		return
	}
	triggerPower("restart-appliance")
}

// runRequests is the headless way to see and decide host requests, for a launcher
// started without the desktop panel. It uses the same authenticated control API.
func runRequests(state, operation, uid string) error {
	c, err := readControl(state)
	if err != nil {
		return errors.New("the appliance is not running for this state directory")
	}
	switch operation {
	case "status", "list", "":
		b, err := controlCall(c, "GET", "/control/host", nil, 30*time.Second)
		if err == nil {
			_, err = os.Stdout.Write(append(b, '\n'))
		}
		return err
	case "approve", "reject":
		if !validRequestUID(uid) {
			return errors.New("--id must be the request UID shown by `requests --operation list`")
		}
		b, err := controlCall(c, "POST", "/control/host/requests/"+uid+"/"+operation, nil, 30*time.Second)
		if err == nil {
			_, err = os.Stdout.Write(b)
		}
		return err
	}
	return errors.New("requests operation must be list, approve or reject")
}

func runAutostart(state, operation string) error {
	switch operation {
	case "enable", "disable":
		if err := setAutostart(state, operation == "enable"); err != nil {
			return err
		}
	case "status", "":
	default:
		return errors.New("autostart operation must be enable, disable or status")
	}
	return json.NewEncoder(os.Stdout).Encode(autostartStatus(state))
}
