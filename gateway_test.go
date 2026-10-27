package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/publication"
)

func testPublication(host string) publication.Record {
	return publication.Record{Name: "web", Namespace: "example", UID: "test-id", Generation: 1, Revision: strings.Repeat("a", 64), Hostname: host, LAN: true, TLS: publication.TLS{Mode: "LocalCA"}}
}

func TestGatewayDefaultsAndListenerConfig(t *testing.T) {
	original := caddySHA256
	caddySHA256 = "packaged"
	t.Cleanup(func() { caddySHA256 = original })
	state := t.TempDir()
	cfg, err := loadGatewayConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	if !((cfg.Port == 443 && cfg.HTTPPort == 80) || (cfg.Port == 8443 && cfg.HTTPPort == 8080)) || !reflect.DeepEqual(cfg.Addresses, []string{wildcardAddress}) {
		t.Fatalf("wrong LAN defaults: %+v", cfg)
	}
	saved := []byte("{\"port\":18443,\"httpPort\":18080,\"addresses\":[\"127.0.0.1\"]}\n")
	path := filepath.Join(state, "gateway.json")
	if err := os.WriteFile(path, saved, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cfg, err = loadGatewayConfig(state)
		if err != nil || cfg.Port != 18443 {
			t.Fatalf("lost listener settings: %+v, %v", cfg, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(saved) {
			t.Fatalf("overwrote saved gateway settings: %v", err)
		}
	}
	if err := os.WriteFile(path, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGatewayConfig(state); err == nil {
		t.Fatal("silently reset invalid saved configuration")
	}
	after, _ := os.ReadFile(path)
	if string(after) != "{invalid" {
		t.Fatal("overwrote invalid saved configuration")
	}
	for _, invalid := range []string{`{"port":443,"httpPort":80,"namespaces":["example"]}`, `{"port":443,"httpPort":80,"mdns":true}`, `{"port":443,"httpPort":80,"domains":["example.local"]}`} {
		var cfg gatewayConfig
		if decodeGatewayConfig([]byte(invalid), &cfg) == nil {
			t.Fatal("accepted removed host publication authority")
		}
	}
}

func TestGatewayDiscoveryRetainsOnErrorAndAppliesDeletion(t *testing.T) {
	record := testPublication("app.example.local")
	g := &gateway{publications: []publication.Record{record}, hosts: []string{"app.example.local"}}
	code, body := 503, ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}}
	defer client.CloseIdleConnections()
	g.refresh(context.Background(), client)
	if !g.permits("app.example.local") || g.err == "" {
		t.Fatal("transient failure lost approved hosts or hid error")
	}
	for _, invalid := range []string{`{}`, `{"items":null}`, `{"items":[{"hostname":"evil.local"}]}`, `{"items":[]} {}`} {
		code, body = 200, invalid
		g.refresh(context.Background(), client)
		if !g.permits(record.Hostname) || g.err == "" {
			t.Fatal("invalid snapshot replaced last valid state")
		}
	}
	code, body = 200, `{"items":[]}`
	g.refresh(context.Background(), client)
	if g.permits("app.example.local") || g.err != "" {
		t.Fatal("successful deletion retained approval")
	}
}

func TestGatewayBridgeDoesNotExposeControl(t *testing.T) {
	g := &gateway{hosts: []string{"app.example.localhost"}, publications: []publication.Record{testPublication("app.example.localhost")}}
	called := false
	h := g.bridgeHandler("secret", "127.0.0.1:1234", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }))
	for _, tc := range []struct {
		host, path, token, method string
		status                    int
	}{
		{"app.example.localhost", "/_agent/stop", "", "POST", 403},
		{"unknown.example.localhost", "/", "secret", "GET", 403},
		{"app.example.localhost", "/_agent/gateway/connect", "secret", "CONNECT", 403},
		{"127.0.0.1:1234", "/secret/ask?domain=unknown.example.localhost", "", "GET", 403},
		{"127.0.0.1:1234", "/secret/ask?domain=app.example.localhost", "", "GET", 200},
		{"app.example.localhost:8443", "/upload", "secret", "POST", 204},
	} {
		called = false
		r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		r.Header.Set("X-CA-Gateway", tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || called != (tc.status == 204) {
			t.Fatalf("%+v: status=%d called=%t", tc, w.Code, called)
		}
	}
}

func TestPublicationLANAndTLSControls(t *testing.T) {
	record := testPublication("app.example.local")
	record.LAN = false
	g := &gateway{config: gatewayConfig{Port: 8443, HTTPPort: 8080}, publications: []publication.Record{record}}
	h := g.bridgeHandler("secret", "127.0.0.1:1234", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != record.Hostname || r.Header.Get("X-Forwarded-Host") != record.Hostname {
			t.Error("lost workload host")
		}
		w.WriteHeader(204)
	}))
	check := func(ip, scheme string, want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "http://"+record.Hostname+"/", nil)
		r.Header.Set("X-CA-Gateway", "secret")
		r.Header.Set("X-Forwarded-For", ip)
		r.Header.Set("X-Forwarded-Proto", scheme)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d != %d", ip, scheme, w.Code, want)
		}
	}
	check("127.0.0.1", "https", 204)
	check("192.168.1.5", "https", 403)
	check("127.0.0.1, 192.168.1.5", "https", 403)
	check("", "https", 403)
	check("127.0.0.1", "http", 308)
	g.publications[0].TLS.Mode = "HTTP"
	check("127.0.0.1", "http", 204)
	check("127.0.0.1", "https", 403)
	g.publications[0].LAN = true
	check("192.168.1.5", "http", 204)
	g.publications = nil
	check("127.0.0.1", "http", 403)
}

func TestCaddyConfigHasNoPublicManagement(t *testing.T) {
	c := caddyConfig(gatewayConfig{Port: 8443}, "store", "127.0.0.1:1234", "secret")
	if !c["admin"].(map[string]any)["disabled"].(bool) {
		t.Fatal("admin enabled")
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "0.0.0.0") || strings.Contains(string(b), `"listen":[":`) {
		t.Fatal(string(b))
	}
	// CI invokes the actual pinned Caddy executable to validate this output.
	if p := os.Getenv("CONTAINERS_TEST_CADDY_CONFIG"); p != "" {
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

type gatewayTestVM struct{ conn net.Conn }

func (v gatewayTestVM) Dial(context.Context, string, string) (net.Conn, error) { return v.conn, nil }
func (v gatewayTestVM) Done() <-chan struct{}                                  { return nil }
func (v gatewayTestVM) Close() error                                           { return nil }

func TestGatewayConnectPreservesBufferedBytes(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go func() {
		buf := make([]byte, 4096)
		b.Read(buf)
		io.WriteString(b, "HTTP/1.1 200 Connection Established\r\n\r\nworkload")
	}()
	c, err := dialGateway(context.Background(), gatewayTestVM{a})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := make([]byte, 8)
	if _, err = io.ReadFull(c, got); err != nil || string(got) != "workload" {
		t.Fatal(string(got), err)
	}
}

func TestGatewayIdentityBackupRoundTrip(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "source")
	pki := filepath.Join(state, "gateway", "caddy", "pki", "authorities", "local")
	if err := os.MkdirAll(pki, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root.key", "root.crt"} {
		if err := os.WriteFile(filepath.Join(pki, name), []byte("identity-"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(state, "data.img"), []byte("disk"), 0600)
	saveJSON(filepath.Join(state, "release.json"), manifest{Arch: runtime.GOARCH, Version: "test"})
	saveJSON(filepath.Join(state, "gateway.json"), gatewayConfig{Port: 8443, HTTPPort: 8080})
	archive := filepath.Join(root, "backup.age")
	if err := backup(state, archive); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "restored")
	if err := restore(dst, archive, archive+".key"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "gateway", "caddy", "pki", "authorities", "local", "root.key"))
	if err != nil || string(b) != "identity-root.key" {
		t.Fatal(string(b), err)
	}
	for _, name := range []string{"../escape", "gateway/caddy/../../escape", "gateway/caddy/C:foo", "gateway/caddy/../x", "gateway/caddy/dir./x", "gateway-run-secret/caddy.json", "gateway/caddy/x\\y"} {
		if validBackupMember(name, 10) {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestBackupWaitsForGatewayAndRefusesMissingCA(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "source")
	lockDir := filepath.Join(state, "gateway-lock")
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockState(lockDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := backup(state, filepath.Join(root, "backup.age")); err == nil {
		t.Fatal("backed up while Caddy supervisor was active")
	}
	store := filepath.Join(state, "gateway", "caddy")
	ca := filepath.Join(store, "pki", "authorities", "local")
	if err := os.MkdirAll(ca, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ca, "root.crt"), []byte("public-only"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkGatewayIdentity(store); err == nil {
		t.Fatal("regenerated missing CA key")
	}
}

func TestGatewayServesThePublicRootCAByAnyHostButNothingElse(t *testing.T) {
	state := t.TempDir()
	pki := filepath.Join(state, "gateway", "caddy", "pki", "authorities", "local")
	if err := os.MkdirAll(pki, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pki, "root.crt"), []byte("public certificate"), 0600)
	os.WriteFile(filepath.Join(pki, "root.key"), []byte("PRIVATE KEY"), 0600)
	g := &gateway{state: state, hosts: []string{"demo.local"}, publications: []publication.Record{testPublication("demo.local")}}
	called := false
	h := g.bridgeHandler("secret", "127.0.0.1:1234", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }))
	for _, tc := range []struct {
		name, host, path, token, method string
		status                          int
		body                            string
	}{
		{"by LAN address", "192.168.220.12", "/root.crt", "secret", "GET", 200, "public certificate"},
		{"by LAN address and port", "192.168.220.12:80", "/root.crt", "secret", "GET", 200, "public certificate"},
		{"by published name", "demo.local", "/root.crt", "secret", "GET", 200, "public certificate"},
		{"without the internal token", "192.168.220.12", "/root.crt", "", "GET", 403, ""},
		{"wrong internal token", "192.168.220.12", "/root.crt", "guess", "GET", 403, ""},
		{"only GET and HEAD", "192.168.220.12", "/root.crt", "secret", "POST", 403, ""},
		{"never the private key", "192.168.220.12", "/root.key", "secret", "GET", 403, ""},
		{"an unpublished host gets nothing else", "192.168.220.12", "/", "secret", "GET", 403, ""},
	} {
		called = false
		r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		r.Header.Set("X-CA-Gateway", tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || called || (tc.body != "" && w.Body.String() != tc.body) || strings.Contains(w.Body.String(), "PRIVATE") {
			t.Fatalf("%s: status=%d called=%t body=%q", tc.name, w.Code, called, w.Body.String())
		}
	}
}
