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
)

func TestGatewayPublicationBoundary(t *testing.T) {
	c := gatewayConfig{Port: 8443, Namespaces: []string{"example"}, Domains: []string{"example.localhost"}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	var snapshot gatewaySnapshot
	err := json.Unmarshal([]byte(`{"items":[
	{"metadata":{"namespace":"example"},"spec":{"ingressClassName":"containers-anywhere","rules":[{"host":"app.example.localhost"},{"host":"evil-example.localhost"},{"host":"*.example.localhost"}]}},
	{"metadata":{"namespace":"other"},"spec":{"ingressClassName":"containers-anywhere","rules":[{"host":"app.example.localhost"}]}},
	{"metadata":{"namespace":"example"},"spec":{"ingressClassName":"other","rules":[{"host":"ignored.example.localhost"}]}}
	]}`), &snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got := publishedHosts(c, snapshot); !reflect.DeepEqual(got, []string{"app.example.localhost"}) {
		t.Fatal(got)
	}
	for _, host := range []string{"UPPER.example.localhost", "app.example.localhost:443", "app.example.localhost/", "127.0.0.1", "app..example.localhost", "app.example.localhost\n"} {
		if c.allows(host, "example") {
			t.Fatalf("accepted %q", host)
		}
	}
	var parsed gatewayConfig
	if err := decodeGatewayConfig([]byte(`{"port":8443,"bind":"0.0.0.0"}`), &parsed); err == nil {
		t.Fatal("silently accepted unknown bind option")
	}
}

func TestGatewayBridgeDoesNotExposeControl(t *testing.T) {
	g := &gateway{hosts: []string{"app.example.localhost"}}
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
	saveJSON(filepath.Join(state, "gateway.json"), gatewayConfig{Port: 8443, Namespaces: []string{"example"}, Domains: []string{"example.localhost"}})
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
