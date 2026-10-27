package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct {
	args  []string
	input string
}

type fakeKube struct {
	mu    sync.Mutex
	calls []call
	// responses for get commands, keyed by resource (args[1]) or "resource/name".
	get  map[string]string
	fail map[string]error // keyed by args[0]+" "+args[1]
}

func (f *fakeKube) run(_ context.Context, input []byte, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{args: args, input: string(input)})
	if err := f.fail[args[0]+" "+args[1]]; err != nil {
		return nil, err
	}
	if args[0] == "get" {
		key := args[1]
		if len(args) > 2 && !strings.HasPrefix(args[2], "-") {
			key += "/" + args[2]
		}
		if out, ok := f.get[key]; ok {
			return []byte(out), nil
		}
		return []byte(`{"items":[]}`), nil
	}
	return nil, nil
}

func (f *fakeKube) patches() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.args[0] == "patch" {
			out = append(out, c)
		}
	}
	return out
}

func newController(k *fakeKube, now *time.Time) *Controller {
	return &Controller{Run: k.run, Now: func() time.Time { return *now }}
}

func goodFacts(at time.Time) Facts {
	return Facts{HostTime: at.UTC().Format(time.RFC3339), AppVersion: "0.3.0", ApplianceVersion: "0.3.0", BundleID: "abc", K3sVersion: "1.35.8+k3s1",
		Arch: "amd64", OS: "linux", CPU: Sizing{Allocated: 2, HostTotal: 8}, MemoryMiB: Sizing{Allocated: 4096, HostTotal: 16384},
		StateDirFreeBytes: 100 << 30, StateDirTotal: 500 << 30, LauncherHealthy: true, Update: Update{Phase: "idle"}}
}

func TestFactsValidationBoundsEveryField(t *testing.T) {
	now := time.Now()
	if err := goodFacts(now).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Facts){
		"bad time":     func(f *Facts) { f.HostTime = "yesterday" },
		"long string":  func(f *Facts) { f.AppVersion = strings.Repeat("x", 300) },
		"newline":      func(f *Facts) { f.OS = "linux\nx" },
		"negative":     func(f *Facts) { f.StateDirFreeBytes = -1 },
		"bad port":     func(f *Facts) { f.Gateway.HTTPSPort = 70000 },
		"many points":  func(f *Facts) { f.Update.RetainedRecoveryPoints = make([]string, 40) },
		"lan too many": func(f *Facts) { f.Gateway.LANCheck = &LANCheck{Results: make([]LANResult, 40)} },
	} {
		f := goodFacts(now)
		mutate(&f)
		if f.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStatusNeverShowsStaleOrMissingHostValues(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	guest := GuestFacts{Disk: DiskFromStatfs(16<<30, 12<<30)}
	if s := Build(now, nil, time.Time{}, guest); s.Host != nil || s.Conditions[0].Status != "Unknown" {
		t.Fatal("never-reported host must be Unknown")
	}
	f := goodFacts(now)
	fresh := Build(now.Add(10*time.Second), &f, now, guest)
	if fresh.Host == nil || fresh.Conditions[0].Status != "True" || fresh.ClockSkewSeconds == nil {
		t.Fatal("fresh facts must be reported")
	}
	stale := Build(now.Add(StaleAfter+time.Second), &f, now, guest)
	if stale.Host != nil || stale.Conditions[0].Reason != "Stale" || *stale.HostFactsAgeSeconds < 30 {
		t.Fatal("stale host facts must become Unknown, not the last value")
	}
}

func TestDiskPressureFlipsWhenTheDiskFills(t *testing.T) {
	if d := DiskFromStatfs(16<<30, 8<<30); d.Pressure || d.UsedBytes != 8<<30 {
		t.Fatalf("%+v", d)
	}
	if d := DiskFromStatfs(16<<30, 500<<20); !d.Pressure || d.PressureReason == "" {
		t.Fatalf("%+v", d)
	}
	if d := DiskFromStatfs(100<<30, 9<<30); !d.Pressure {
		t.Fatal("10 percent floor must apply to a large disk")
	}
	if d := DiskFromStatfs(10, 20); d.FreeBytes != 10 || d.UsedBytes != 0 {
		t.Fatalf("free must be clamped: %+v", d)
	}
}

func TestRequestSpecValidation(t *testing.T) {
	ok := []RequestSpec{
		{Type: TypeResources, Resources: &ResourcesSpec{CPUs: 4, MemoryMiB: 8192}},
		{Type: TypePower, Power: &PowerSpec{Action: "shutdown"}},
		{Type: TypePower, Power: &PowerSpec{Action: "restart-appliance"}},
		{Type: TypeAutostart, Autostart: &AutostartSpec{Enabled: true}},
		{Type: TypePruneImages}, {Type: TypeLANCheck},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := []RequestSpec{
		{Type: "format-disk"}, {Type: TypeResources},
		{Type: TypeResources, Resources: &ResourcesSpec{CPUs: 1, MemoryMiB: 8192}},
		{Type: TypeResources, Resources: &ResourcesSpec{CPUs: 2, MemoryMiB: 512}},
		{Type: TypeResources, Resources: &ResourcesSpec{CPUs: 2, MemoryMiB: 1 << 30}},
		{Type: TypeResources, Resources: &ResourcesSpec{CPUs: 2, MemoryMiB: 4096}, Power: &PowerSpec{Action: "shutdown"}},
		{Type: TypePower, Power: &PowerSpec{Action: "reboot-host"}},
		{Type: TypePruneImages, Autostart: &AutostartSpec{}},
		{Type: TypeLANCheck, Reason: "x\ny"},
	}
	for _, s := range bad {
		if s.Validate() == nil {
			t.Errorf("accepted %+v", s)
		}
	}
}

func TestPhaseMachineNeverReopensATerminalRequest(t *testing.T) {
	for _, from := range []string{PhaseRejected, PhaseApplied, PhaseFailed, PhaseExpired} {
		for _, to := range []string{PhasePending, PhaseApproved, PhaseApplied, PhaseFailed, PhaseRejected} {
			if CanTransition(from, to) {
				t.Errorf("%s -> %s allowed", from, to)
			}
		}
	}
	if !CanTransition(PhasePending, PhaseApproved) || !CanTransition(PhaseApproved, PhaseApplied) || CanTransition(PhaseApproved, PhaseRejected) {
		t.Fatal("unexpected transitions")
	}
}

func TestStatusIsWrittenThroughTheStatusSubresourceAndNotChurned(t *testing.T) {
	k := &fakeKube{}
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	c := newController(k, &now)
	f := goodFacts(now)
	if err := c.SetFacts(f); err != nil {
		t.Fatal(err)
	}
	guest := GuestFacts{UptimeSeconds: 5, Disk: DiskFromStatfs(16<<30, 12<<30)}
	if err := c.ReconcileStatus(context.Background(), guest); err != nil {
		t.Fatal(err)
	}
	if len(k.calls) != 2 || k.calls[0].args[0] != "apply" || k.calls[1].args[0] != "patch" || !strings.Contains(strings.Join(k.calls[1].args, " "), "--subresource=status") {
		t.Fatalf("calls: %+v", k.calls)
	}
	if !strings.Contains(k.calls[0].input, `"kind":"HostStatus"`) || strings.Contains(k.calls[0].input, "status") && strings.Contains(k.calls[0].input, `"status":`) {
		t.Fatal("the apply must create the object without status")
	}
	now = now.Add(5 * time.Second)
	guest.UptimeSeconds = 10
	if err := c.ReconcileStatus(context.Background(), guest); err != nil || len(k.calls) != 2 {
		t.Fatalf("an unchanged status was rewritten: %d calls, %v", len(k.calls), err)
	}
	guest.Disk = DiskFromStatfs(16<<30, 100<<20)
	if err := c.ReconcileStatus(context.Background(), guest); err != nil || len(k.calls) != 4 {
		t.Fatal("a material change must be written immediately")
	}
	now = now.Add(31 * time.Second)
	if err := c.ReconcileStatus(context.Background(), guest); err != nil || len(k.calls) != 6 {
		t.Fatal("the status must be refreshed at least every 30 seconds")
	}
}

func TestStatusWriteFailureIsReportedAndRetried(t *testing.T) {
	k := &fakeKube{fail: map[string]error{"apply --server-side": errors.New("crd missing")}}
	now := time.Now()
	c := newController(k, &now)
	if err := c.ReconcileStatus(context.Background(), GuestFacts{}); err == nil {
		t.Fatal("expected the error")
	}
	if s := c.Snapshot(); s.Written || s.LastError == "" {
		t.Fatalf("%+v", s)
	}
	delete(k.fail, "apply --server-side")
	if err := c.ReconcileStatus(context.Background(), GuestFacts{}); err != nil || !c.Snapshot().Written {
		t.Fatal("must retry and recover")
	}
}

func requestJSON(items ...Request) string {
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b)
}

func newRequest(uid, rv, phase string, spec RequestSpec, created time.Time) Request {
	return Request{Metadata: Meta{Name: "r-" + uid, Namespace: "ns", UID: uid, ResourceVersion: rv, Generation: 1, CreationTimestamp: created.UTC().Format(time.RFC3339)},
		Spec: spec, Status: RequestStatus{Phase: phase, ObservedGeneration: 1}}
}

func TestNewRequestsBecomePendingWithUIDAndVersionGuards(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	good := newRequest("u1", "7", "", RequestSpec{Type: TypePruneImages}, now)
	good.Status = RequestStatus{}
	bad := newRequest("u2", "8", "", RequestSpec{Type: "format-disk"}, now)
	bad.Status = RequestStatus{}
	done := newRequest("u3", "9", PhaseApplied, RequestSpec{Type: TypePruneImages}, now)
	k := &fakeKube{get: map[string]string{RequestResource: requestJSON(good, bad, done)}}
	c := newController(k, &now)
	if err := c.ReconcileRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	patches := k.patches()
	if len(patches) != 2 {
		t.Fatalf("want 2 patches (pending + rejected), got %d", len(patches))
	}
	for _, p := range patches {
		joined := strings.Join(p.args, " ")
		if !strings.Contains(joined, "--subresource=status") || !strings.Contains(joined, `"/metadata/uid"`) || !strings.Contains(joined, `"/metadata/resourceVersion"`) {
			t.Fatalf("patch lacks guards: %s", joined)
		}
	}
	if !strings.Contains(strings.Join(patches[0].args, " "), PhasePending) || !strings.Contains(strings.Join(patches[1].args, " "), "InvalidRequest") {
		t.Fatal("unexpected phases")
	}
	act := c.Actionable()
	if len(act) != 1 || act[0].UID != "u1" || act[0].Phase != PhasePending {
		t.Fatalf("only the valid new request is actionable: %+v", act)
	}
}

func TestChangedGenerationInvalidatesAnOldDecision(t *testing.T) {
	now := time.Now()
	r := newRequest("u1", "7", PhaseApproved, RequestSpec{Type: TypePruneImages}, now)
	r.Metadata.Generation = 2 // spec changed after the approval was recorded for generation 1
	k := &fakeKube{get: map[string]string{RequestResource: requestJSON(r)}}
	c := newController(k, &now)
	if err := c.ReconcileRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Actionable(); len(got) != 1 || got[0].Phase != PhasePending {
		t.Fatalf("a changed spec must go back to Pending, got %+v", got)
	}
}

func TestOldPendingRequestsExpire(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	r := newRequest("u1", "7", PhasePending, RequestSpec{Type: TypePruneImages}, now.Add(-25*time.Hour))
	k := &fakeKube{get: map[string]string{RequestResource: requestJSON(r)}}
	c := newController(k, &now)
	if err := c.ReconcileRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := k.patches(); len(p) != 1 || !strings.Contains(strings.Join(p[0].args, " "), PhaseExpired) || len(c.Actionable()) != 0 {
		t.Fatal("a stale pending request must expire and stop being actionable")
	}
}

func decisionFixture(t *testing.T, phase string) (*fakeKube, *Controller, Request) {
	t.Helper()
	now := time.Now()
	r := newRequest("u1", "7", phase, RequestSpec{Type: TypePower, Power: &PowerSpec{Action: "shutdown"}}, now)
	k := &fakeKube{get: map[string]string{RequestResource: requestJSON(r)}}
	c := newController(k, &now)
	if err := c.ReconcileRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Metadata.ResourceVersion = "9"
	b, _ := json.Marshal(r)
	k.get[RequestResource+"/r-u1"] = string(b)
	k.calls = nil
	return k, c, r
}

func TestDecisionsLandOnlyOnTheExactObjectAndFollowThePhaseMachine(t *testing.T) {
	k, c, _ := decisionFixture(t, PhasePending)
	if err := c.SetDecisions(context.Background(), []Decision{{UID: "u1", Phase: PhaseApproved}}); err != nil {
		t.Fatal(err)
	}
	p := k.patches()
	if len(p) != 1 || !strings.Contains(strings.Join(p[0].args, " "), `"9"`) {
		t.Fatal("the decision must be patched against the freshly read resourceVersion")
	}
	if got := c.Actionable(); len(got) != 1 || got[0].Phase != PhaseApproved {
		t.Fatalf("approved stays actionable until applied: %+v", got)
	}
	if err := c.SetDecisions(context.Background(), []Decision{{UID: "u1", Phase: PhaseRejected}}); err == nil {
		t.Fatal("an approved request cannot be rejected")
	}
	// The cluster now reflects the approval, as a real one would.
	_, _, approved := decisionFixture(t, PhaseApproved)
	approved.Metadata.ResourceVersion = "10"
	raw, _ := json.Marshal(approved)
	k.get[RequestResource+"/r-u1"] = string(raw)
	if err := c.SetDecisions(context.Background(), []Decision{{UID: "u1", Phase: PhaseApplied, Result: json.RawMessage(`{"ok":true}`)}}); err != nil {
		t.Fatal(err)
	}
	if len(c.Actionable()) != 0 {
		t.Fatal("an applied request is done")
	}
	if err := c.SetDecisions(context.Background(), []Decision{{UID: "u1", Phase: PhaseApproved}}); err == nil {
		t.Fatal("a completed request must not accept another decision")
	}
}

func TestDecisionOnARecreatedNamesakeIsDiscarded(t *testing.T) {
	k, c, r := decisionFixture(t, PhasePending)
	r.Metadata.UID = "someone-else"
	b, _ := json.Marshal(r)
	k.get[RequestResource+"/r-u1"] = string(b)
	if err := c.SetDecisions(context.Background(), []Decision{{UID: "u1", Phase: PhaseApproved}}); err == nil || len(k.patches()) != 0 {
		t.Fatal("a decision must never land on a different object with the same name")
	}
}

func TestInvalidDecisionsAreRefused(t *testing.T) {
	_, c, _ := decisionFixture(t, PhasePending)
	for _, d := range []Decision{{UID: "", Phase: PhaseApproved}, {UID: "u1", Phase: PhasePending}, {UID: "u1", Phase: "Whatever"},
		{UID: "u1", Phase: PhaseApplied, Result: json.RawMessage(`{`)}, {UID: "unknown", Phase: PhaseApproved}} {
		if c.SetDecisions(context.Background(), []Decision{d}) == nil {
			t.Errorf("accepted %+v", d)
		}
	}
}

func TestEndpointValidation(t *testing.T) {
	local := []net.IP{net.ParseIP("10.0.2.15")}
	for _, ok := range []string{"127.0.0.1:30500", "localhost:5000", "10.0.2.15:5000", "[::1]:5000"} {
		if err := ValidateEndpoint(ok, local); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "registry.example.com:5000", "*:5000", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:05000",
		"http://127.0.0.1:5000", "127.0.0.1:5000/v2", "user@127.0.0.1:5000", "8.8.8.8:5000", "10.43.0.5:5000", "127.0.0.1:50 00", "0.0.0.0:5000"} {
		if ValidateEndpoint(bad, local) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func trustJSON(items ...Trust) string {
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b)
}

func trust(uid, endpoint string, http bool) Trust {
	return Trust{Metadata: Meta{Name: "t-" + uid, UID: uid, ResourceVersion: "1", Generation: 1}, Spec: TrustSpec{Endpoint: endpoint, PlainHTTP: http}}
}

func TestRegistryTrustWritesRemovesAndOnlyTouchesWhatItOwns(t *testing.T) {
	dir := t.TempDir()
	certs, state := filepath.Join(dir, "certs.d"), filepath.Join(dir, "state", "registry-trust.json")
	foreign := filepath.Join(certs, "127.0.0.1:9999")
	os.MkdirAll(foreign, 0755)
	os.WriteFile(filepath.Join(foreign, "hosts.toml"), []byte("# someone else\nserver = \"https://x\"\n"), 0644)
	k := &fakeKube{get: map[string]string{RegistryTrustResource: trustJSON(trust("a", "127.0.0.1:30500", true), trust("b", "evil.example.com:5000", true), trust("c", "127.0.0.1:30501", false))}}
	now := time.Now()
	c := newController(k, &now)
	if err := c.ReconcileRegistryTrust(context.Background(), certs, state, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(certs, "127.0.0.1:30500", "hosts.toml"))
	if err != nil || !strings.Contains(string(b), `server = "http://127.0.0.1:30500"`) || !strings.HasPrefix(string(b), registryMarker) {
		t.Fatalf("hosts.toml: %q %v", b, err)
	}
	for _, name := range []string{"evil.example.com:5000", "127.0.0.1:30501"} {
		if _, err := os.Stat(filepath.Join(certs, name)); err == nil {
			t.Fatalf("%s must not be trusted", name)
		}
	}
	reasons := ""
	for _, p := range k.patches() {
		reasons += strings.Join(p.args, " ")
	}
	for _, want := range []string{"Trusted", "InvalidEndpoint", "Unsupported"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("status lacks %s", want)
		}
	}
	// Removal revokes trust, but only files this agent wrote.
	k.get[RegistryTrustResource] = trustJSON()
	if err := c.ReconcileRegistryTrust(context.Background(), certs, state, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(certs, "127.0.0.1:30500")); err == nil {
		t.Fatal("removal must revoke trust")
	}
	if _, err := os.Stat(filepath.Join(foreign, "hosts.toml")); err != nil {
		t.Fatal("configuration the agent did not write must survive")
	}
}

func TestRegistryTrustRefusesToReplaceForeignConfigurationAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	certs := filepath.Join(dir, "certs.d")
	foreign := filepath.Join(certs, "127.0.0.1:30500")
	os.MkdirAll(foreign, 0755)
	os.WriteFile(filepath.Join(foreign, "hosts.toml"), []byte("# not ours\n"), 0644)
	k := &fakeKube{get: map[string]string{RegistryTrustResource: trustJSON(trust("a", "127.0.0.1:30500", true))}}
	now := time.Now()
	c := newController(k, &now)
	if err := c.ReconcileRegistryTrust(context.Background(), certs, filepath.Join(dir, "s.json"), nil); err == nil {
		t.Fatal("must refuse to overwrite foreign configuration")
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "hosts.toml")); string(b) != "# not ours\n" {
		t.Fatal("foreign file was modified")
	}
	k.get[RegistryTrustResource] = trustJSON(trust("a", "127.0.0.1:30600", true), trust("b", "127.0.0.1:30600", true))
	c.ReconcileRegistryTrust(context.Background(), certs, filepath.Join(dir, "s.json"), nil)
	if _, err := os.Stat(filepath.Join(certs, "127.0.0.1:30600")); err == nil {
		t.Fatal("competing claims must fail closed")
	}
}

func TestMeasurementHelpers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), make([]byte, 1000), 0600)
	os.MkdirAll(filepath.Join(dir, "sub"), 0700)
	os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 500), 0600)
	os.Symlink("/etc/hosts", filepath.Join(dir, "link"))
	if n, complete := DirSize(context.Background(), dir, time.Second); n != 1500 || !complete {
		t.Fatalf("size=%d complete=%v", n, complete)
	}
	if n, complete := DirSize(context.Background(), filepath.Join(dir, "missing"), time.Second); n != 0 || !complete {
		t.Fatalf("missing dir: %d %v", n, complete)
	}
	total, err := ImageBytes([]byte(`{"images":[{"id":"a","size":"1000"},{"id":"b","size":2000}]}`))
	if err != nil || total != 3000 {
		t.Fatal(total, err)
	}
	if _, err := ImageBytes([]byte(`{"images":[{"size":"x"}]}`)); err == nil {
		t.Fatal("garbage sizes must be rejected")
	}
	if r := NewPruneResult(5000, 3000); r.Reclaimed != 2000 {
		t.Fatal(r)
	}
	if r := NewPruneResult(3000, 5000); r.Reclaimed != 0 {
		t.Fatal("never report negative reclaim")
	}
}

func TestAgentRoutesBoundAndValidateInput(t *testing.T) {
	k := &fakeKube{}
	now := time.Now()
	c := newController(k, &now)
	mux := http.NewServeMux()
	c.Routes(mux)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	good, _ := json.Marshal(goodFacts(now))
	if w := send("PUT", "/_agent/host/facts", string(good)); w.Code != 204 {
		t.Fatalf("good facts: %d", w.Code)
	}
	if w := send("PUT", "/_agent/host/facts", `{"hostTime":"2026-01-01T00:00:00Z","surprise":1}`); w.Code != 400 {
		t.Fatalf("unknown fields must be rejected: %d", w.Code)
	}
	if w := send("PUT", "/_agent/host/facts", string(good)+"{}"); w.Code != 400 {
		t.Fatalf("trailing data must be rejected: %d", w.Code)
	}
	if w := send("PUT", "/_agent/host/facts", strings.Repeat("a", 70<<10)); w.Code != 400 {
		t.Fatalf("oversize body: %d", w.Code)
	}
	if w := send("GET", "/_agent/host/requests", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"requests":[]`) {
		t.Fatal(w.Body.String())
	}
	if w := send("POST", "/_agent/host/decisions", `{"decisions":[{"uid":"nope","phase":"Approved"}]}`); w.Code != 409 {
		t.Fatalf("unknown request: %d", w.Code)
	}
	many := `{"decisions":[` + strings.Repeat(`{"uid":"a","phase":"Approved"},`, 70) + `{"uid":"a","phase":"Approved"}]}`
	if w := send("POST", "/_agent/host/decisions", many); w.Code != 400 {
		t.Fatalf("too many decisions: %d", w.Code)
	}
}
