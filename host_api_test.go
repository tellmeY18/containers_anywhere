package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/hostapi"
)

type fakeGuest struct {
	mu        sync.Mutex
	requests  []hostapi.Actionable
	decisions []hostapi.Decision
	facts     []hostapi.Facts
	pruned    int
}

func (g *fakeGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "PUT /_agent/host/facts":
		var f hostapi.Facts
		if json.NewDecoder(r.Body).Decode(&f) != nil || f.Validate() != nil {
			http.Error(w, "bad facts", 400)
			return
		}
		g.facts = append(g.facts, f)
		w.WriteHeader(204)
	case "GET /_agent/host/requests":
		json.NewEncoder(w).Encode(map[string]any{"requests": g.requests})
	case "POST /_agent/host/decisions":
		var body struct{ Decisions []hostapi.Decision }
		json.NewDecoder(r.Body).Decode(&body)
		g.decisions = append(g.decisions, body.Decisions...)
		w.WriteHeader(204)
	case "POST /_agent/host/prune-images":
		g.pruned++
		io.WriteString(w, `{"imageBytesBefore":5000,"imageBytesAfter":3000,"reclaimedBytes":2000}`)
	case "GET /_agent/host/status":
		io.WriteString(w, `{"status":{"observedAt":"x"},"written":true}`)
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGuest) phases() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, d := range g.decisions {
		out = append(out, d.UID+":"+d.Phase)
	}
	return out
}

type rewriteTransport struct{ target *httptest.Server }

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(t.target.URL, "http://")
	return http.DefaultTransport.RoundTrip(r2)
}

func newBridgeFixture(t *testing.T) (*hostBridge, *fakeGuest, string) {
	t.Helper()
	state := t.TempDir()
	guest := &fakeGuest{}
	srv := httptest.NewServer(guest)
	t.Cleanup(srv.Close)
	_, m := testBundle(t)
	client := &http.Client{Transport: rewriteTransport{srv}, Timeout: 10 * time.Second}
	return newHostBridge(state, m, nil, client), guest, state
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func act(uid, typ string, spec hostapi.RequestSpec, phase string) hostapi.Actionable {
	spec.Type = typ
	return hostapi.Actionable{UID: uid, Namespace: "ns", Name: "r", Generation: 1, Created: "2026-10-28T10:00:00Z", Phase: phase, Spec: spec}
}

func TestFactsAreValidAndTruthful(t *testing.T) {
	b, guest, state := newBridgeFixture(t)
	if err := b.push(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := guest.facts[0]
	if f.Arch != b.m.Arch || f.OS != runtime.GOOS || f.K3sVersion != b.m.K3s || f.BundleID != bundleID(b.m) || f.CPU.Allocated != 2 || f.MemoryMiB.Allocated != 4096 {
		t.Fatalf("%+v", f)
	}
	if f.StateDirFreeBytes <= 0 || f.StateDirTotal < f.StateDirFreeBytes {
		t.Fatalf("free=%d total=%d", f.StateDirFreeBytes, f.StateDirTotal)
	}
	if f.Update.Phase != "idle" || f.Gateway.Enabled {
		t.Fatalf("%+v", f)
	}
	for _, secret := range []string{state, "token", "BEGIN"} {
		raw, _ := json.Marshal(f)
		if strings.Contains(string(raw), secret) {
			t.Fatalf("facts leak %q", secret)
		}
	}
}

func TestPendingResizeIsReportedAsRequestedNotAllocated(t *testing.T) {
	b, guest, state := newBridgeFixture(t)
	if err := saveJSON(filepath.Join(state, approvedResourcesFile), approvedResources{CPUs: 4, MemoryMiB: 8192}); err != nil {
		t.Fatal(err)
	}
	b.push(context.Background())
	f := guest.facts[0]
	if f.CPU.Allocated != 2 || f.CPU.Requested == nil || *f.CPU.Requested != 4 || *f.MemoryMiB.Requested != 8192 {
		t.Fatalf("%+v", f)
	}
}

func TestUpdateFactMirrorsTheJournalAndNeverClaimsMore(t *testing.T) {
	state := t.TempDir()
	if u := hostUpdateFact(state, "0.2.0"); u.Phase != "idle" || u.BlockedReason != "" {
		t.Fatalf("%+v", u)
	}
	mustSave(t, filepath.Join(state, "update.json"), updateJournal{Format: 1, ID: "transaction-1", Phase: "prepared", Candidate: manifest{Version: "0.2.1"}})
	os.MkdirAll(filepath.Join(state, "updates", "transaction-1"), 0700)
	u := hostUpdateFact(state, "0.2.0")
	if u.Kind != "transaction" || u.Phase != "prepared" || u.CandidateVersion != "0.2.1" || !strings.Contains(u.BlockedReason, "ordinary startup is blocked") || len(u.RetainedRecoveryPoints) != 1 {
		t.Fatalf("a pending update must say why startup is blocked: %+v", u)
	}
	os.Remove(filepath.Join(state, "update.json"))
	mustSave(t, filepath.Join(state, "swap.json"), swapJournal{Format: 1, ID: "swap-abc", Phase: "rolled-back", Candidate: manifest{Version: "0.2.2"}})
	if u := hostUpdateFact(state, "0.2.0"); u.Kind != "swap" || u.Phase != "rolled-back" {
		t.Fatalf("%+v", u)
	}
}

func TestCertificateFingerprintMatchesOpenSSLFormat(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	path := filepath.Join(t.TempDir(), "root.crt")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	fp, err := certFingerprint(path)
	if err != nil || len(fp) != 32*3-1 || strings.ToUpper(fp) != fp || strings.Count(fp, ":") != 31 {
		t.Fatalf("%q %v", fp, err)
	}
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("secret")}), 0600)
	if _, err := certFingerprint(path); err == nil {
		t.Fatal("a private key must never be treated as the CA certificate")
	}
}

func TestLANCheckIsAutoApprovedAndHonestAboutItsLimits(t *testing.T) {
	b, guest, _ := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-1", hostapi.TypeLANCheck, hostapi.RequestSpec{}, hostapi.PhasePending)}
	if err := b.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "lan-check decisions", func() bool { return len(guest.phases()) == 2 })
	if p := guest.phases(); p[0] != "aaaaaaaa-1:Approved" || p[1] != "aaaaaaaa-1:Applied" {
		t.Fatalf("%v", p)
	}
	last := guest.decisions[1]
	if !strings.Contains(string(last.Result), "does not prove that other devices") {
		t.Fatalf("result must state its limits: %s", last.Result)
	}
	b.mu.Lock()
	hasLAN := b.lan != nil
	b.mu.Unlock()
	if !hasLAN {
		t.Fatal("the check result must feed HostStatus")
	}
}

func TestNothingElseIsApprovedWithoutAPerson(t *testing.T) {
	b, guest, _ := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{
		act("aaaaaaaa-2", hostapi.TypeResources, hostapi.RequestSpec{Resources: &hostapi.ResourcesSpec{CPUs: 2, MemoryMiB: 2048}}, hostapi.PhasePending),
		act("aaaaaaaa-3", hostapi.TypePower, hostapi.RequestSpec{Power: &hostapi.PowerSpec{Action: "shutdown"}}, hostapi.PhasePending),
		act("aaaaaaaa-4", hostapi.TypePruneImages, hostapi.RequestSpec{}, hostapi.PhasePending),
		act("aaaaaaaa-5", hostapi.TypeAutostart, hostapi.RequestSpec{Autostart: &hostapi.AutostartSpec{Enabled: true}}, hostapi.PhasePending),
	}
	b.fetch(context.Background())
	time.Sleep(200 * time.Millisecond)
	if p := guest.phases(); len(p) != 0 {
		t.Fatalf("the launcher approved without a person: %v", p)
	}
	if n := len(b.pending()); n != 4 {
		t.Fatalf("all four must wait for approval, got %d", n)
	}
}

func TestApprovedResizeIsPersistedForTheNextStartOnly(t *testing.T) {
	if total, err := hostMemoryMiB(); err != nil || total < 4096+2048 {
		t.Skip("needs a host with at least 6 GiB")
	}
	b, guest, state := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-6", hostapi.TypeResources, hostapi.RequestSpec{Resources: &hostapi.ResourcesSpec{CPUs: 2, MemoryMiB: 4096}}, hostapi.PhasePending)}
	b.fetch(context.Background())
	if err := b.Approve("aaaaaaaa-6"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "applied", func() bool { return len(guest.phases()) == 2 })
	if p := guest.phases(); p[0] != "aaaaaaaa-6:Approved" || p[1] != "aaaaaaaa-6:Applied" {
		t.Fatalf("%v", p)
	}
	t.Setenv("CONTAINERS_CPUS", "")
	t.Setenv("CONTAINERS_MEMORY_MIB", "")
	cpus, mem, err := vmResourcesFor(state)
	if err != nil || cpus != 2 || mem != 4096 {
		t.Fatalf("%d %d %v", cpus, mem, err)
	}
	if b.cpus != 2 || b.memoryMiB != 4096 {
		t.Fatal("the running appliance must not be resized")
	}
}

func TestOversubscribedResizeIsRefusedAndLeavesTheConfigurationAlone(t *testing.T) {
	b, guest, state := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-7", hostapi.TypeResources, hostapi.RequestSpec{Resources: &hostapi.ResourcesSpec{CPUs: 64, MemoryMiB: 262144}}, hostapi.PhasePending)}
	b.fetch(context.Background())
	if err := b.Approve("aaaaaaaa-7"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "refusal", func() bool { return len(guest.phases()) == 1 })
	if p := guest.phases(); p[0] != "aaaaaaaa-7:Rejected" {
		t.Fatalf("%v", p)
	}
	if _, err := os.Stat(filepath.Join(state, approvedResourcesFile)); err == nil {
		t.Fatal("a refused request must not change the configuration")
	}
}

func TestPowerIsRecordedBeforeItActsAndNeverReplayed(t *testing.T) {
	for len(hostPower) > 0 {
		<-hostPower
	}
	restartAfterStop.Store(false)
	b, guest, _ := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-8", hostapi.TypePower, hostapi.RequestSpec{Power: &hostapi.PowerSpec{Action: "restart-appliance"}}, hostapi.PhasePending)}
	b.fetch(context.Background())
	if err := b.Approve("aaaaaaaa-8"); err != nil {
		t.Fatal(err)
	}
	select {
	case action := <-hostPower:
		if action != "restart-appliance" || !restartAfterStop.Load() {
			t.Fatal("restart must be flagged so serve() starts it again")
		}
		if p := guest.phases(); len(p) != 2 || p[1] != "aaaaaaaa-8:Applied" {
			t.Fatalf("Applied must be recorded before the action runs: %v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the power action never ran")
	}
	restartAfterStop.Store(false)
	// An approved power request found after a launcher restart is never replayed.
	guest.mu.Lock()
	guest.decisions = nil
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-9", hostapi.TypePower, hostapi.RequestSpec{Power: &hostapi.PowerSpec{Action: "shutdown"}}, hostapi.PhaseApproved)}
	guest.mu.Unlock()
	b.fetch(context.Background())
	waitFor(t, "interrupted", func() bool { return len(guest.phases()) == 1 })
	if p := guest.phases(); p[0] != "aaaaaaaa-9:Failed" || len(hostPower) != 0 {
		t.Fatalf("%v", p)
	}
}

func TestPruneCallsTheGuestOnlyAfterApproval(t *testing.T) {
	b, guest, _ := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-a", hostapi.TypePruneImages, hostapi.RequestSpec{}, hostapi.PhasePending)}
	b.fetch(context.Background())
	time.Sleep(100 * time.Millisecond)
	guest.mu.Lock()
	n := guest.pruned
	guest.mu.Unlock()
	if n != 0 {
		t.Fatal("prune ran without approval")
	}
	b.Approve("aaaaaaaa-a")
	waitFor(t, "prune applied", func() bool { return len(guest.phases()) == 2 })
	guest.mu.Lock()
	defer guest.mu.Unlock()
	if guest.pruned != 1 || !strings.Contains(string(guest.decisions[1].Result), "reclaimedBytes") {
		t.Fatalf("pruned=%d %s", guest.pruned, guest.decisions[1].Result)
	}
}

func TestRejectAndUnknownRequests(t *testing.T) {
	b, guest, _ := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-b", hostapi.TypePruneImages, hostapi.RequestSpec{}, hostapi.PhasePending)}
	b.fetch(context.Background())
	if err := b.Reject("aaaaaaaa-b"); err != nil {
		t.Fatal(err)
	}
	if p := guest.phases(); len(p) != 1 || p[0] != "aaaaaaaa-b:Rejected" {
		t.Fatalf("%v", p)
	}
	if b.Approve("does-not-exist") == nil || b.Reject("does-not-exist") == nil {
		t.Fatal("unknown requests must be refused")
	}
	if validRequestUID("../../etc") || validRequestUID("a b") || !validRequestUID("0f8fad5b-d9cb-469f-a165-70867728950e") {
		t.Fatal("request UID validation")
	}
}

func TestAutostartRequestUsesTheLauncherWithoutTouchingTheRealUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", home)
	orig := runAutostartCommand
	runAutostartCommand = func(name string, args ...string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { runAutostartCommand = orig })
	b, guest, _ := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-c", hostapi.TypeAutostart, hostapi.RequestSpec{Autostart: &hostapi.AutostartSpec{Enabled: false}}, hostapi.PhasePending)}
	b.fetch(context.Background())
	b.Approve("aaaaaaaa-c")
	waitFor(t, "autostart decision", func() bool { return len(guest.phases()) == 2 })
	if p := guest.phases(); p[0] != "aaaaaaaa-c:Approved" || !strings.HasSuffix(p[1], ":Applied") && !strings.HasSuffix(p[1], ":Failed") {
		t.Fatalf("%v", p)
	}
}

func TestDescribeCombinesHostFactsPendingAndGuestStatus(t *testing.T) {
	b, guest, _ := newBridgeFixture(t)
	guest.requests = []hostapi.Actionable{act("aaaaaaaa-d", hostapi.TypePruneImages, hostapi.RequestSpec{}, hostapi.PhasePending)}
	b.fetch(context.Background())
	raw, err := b.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil || out["facts"] == nil || out["agent"] == nil || !strings.Contains(string(out["pending"]), "aaaaaaaa-d") {
		t.Fatalf("%s", raw)
	}
}
