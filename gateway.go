package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/publication"
)

// Packaging pins the exact adjacent helper, including its final macOS signature.
var caddySHA256 string

type gatewayConfig struct {
	Port      int      `json:"port"`
	HTTPPort  int      `json:"httpPort,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

func (c gatewayConfig) validate() error {
	if c.Port < 1 || c.Port > 65535 || c.HTTPPort < 1 || c.HTTPPort > 65535 || c.Port == c.HTTPPort {
		return errors.New("gateway requires distinct HTTP and HTTPS ports 1..65535")
	}
	if len(c.Addresses) > 16 {
		return errors.New("too many gateway addresses")
	}
	if slices.Contains(c.Addresses, wildcardAddress) && len(c.Addresses) != 1 {
		return errors.New("wildcard gateway address cannot be combined with other addresses")
	}
	for _, address := range c.Addresses {
		if address == wildcardAddress {
			continue
		}
		ip := net.ParseIP(address)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() || (!ip.IsLoopback() && !ip.IsPrivate()) {
			return fmt.Errorf("gateway address %q must be explicit loopback or private LAN IPv4", address)
		}
	}
	if len(c.Addresses) > 0 && !slices.Contains(c.Addresses, "127.0.0.1") && !slices.Contains(c.Addresses, wildcardAddress) {
		return errors.New("gateway probes require a loopback listener")
	}
	return nil
}

func (c gatewayConfig) listeners(port int) []string {
	addresses := c.Addresses
	if len(addresses) == 0 {
		addresses = []string{"127.0.0.1"}
	}
	var listeners []string
	for _, address := range addresses {
		listeners = append(listeners, net.JoinHostPort(address, fmt.Sprint(port)))
	}
	return listeners
}

func configureGateway(state, file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var c gatewayConfig
	if err = decodeGatewayConfig(b, &c); err != nil {
		return err
	}
	if err = c.validate(); err != nil {
		return err
	}
	if err = os.MkdirAll(state, 0700); err != nil {
		return err
	}
	unlock, err := lockState(state)
	if err != nil {
		return err
	}
	defer unlock()
	return saveJSON(filepath.Join(state, "gateway.json"), c)
}

func decodeGatewayConfig(b []byte, c *gatewayConfig) error {
	if len(b) > 1<<20 {
		return errors.New("gateway config exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(c); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("gateway config must contain one JSON object")
	}
	return nil
}

type gateway struct {
	mu             sync.RWMutex
	hosts          []string
	publications   []publication.Record
	err            string
	config         gatewayConfig
	server         *http.Server
	cancel         context.CancelFunc
	done           chan struct{}
	child          *exec.Cmd
	exited         chan struct{}
	log            *os.File
	runDir         string
	input          io.WriteCloser
	childError     string
	discoveryError string
	advertiser     *demoAdvertiser
	endpoints      []gatewayEndpoint
	state          string
}

type gatewayEndpoint struct {
	URL              string `json:"url"`
	Ready            bool   `json:"ready"`
	Error            string `json:"error,omitempty"`
	Advertised       bool   `json:"advertised"`
	CertificateReady bool   `json:"certificateReady"`
}

func (g *gateway) status() map[string]any {
	g.mu.RLock()
	defer g.mu.RUnlock()
	urls := []string{}
	for _, record := range g.publications {
		urls = append(urls, strings.TrimSuffix(g.endpoint(record).url(), "/"))
	}
	demoURL := ""
	for _, endpoint := range g.endpoints {
		if endpoint.Ready && strings.HasPrefix(endpoint.URL, "http://demo.local:") {
			demoURL = endpoint.URL
		}
	}
	return map[string]any{"enabled": true, "listen": g.config.listeners(g.config.Port), "urls": urls, "endpoints": append([]gatewayEndpoint{}, g.endpoints...), "error": g.err, "processError": g.childError, "discoveryError": g.discoveryError, "demoURL": demoURL, "certificateTrust": "manual"}
}

func (g *gateway) permits(host string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return slices.Contains(g.hosts, host)
}

func (g *gateway) record(host string) (publication.Record, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, record := range g.publications {
		if record.Hostname == host {
			return record, true
		}
	}
	return publication.Record{}, false
}

func (g *gateway) endpoint(record publication.Record) mdnsEndpoint {
	e := mdnsEndpoint{Host: record.Hostname, Port: uint16(g.config.Port)}
	if record.TLS.Mode == "HTTP" {
		e.HTTP, e.Port = true, uint16(g.config.HTTPPort)
	}
	return e
}

func loadGatewayConfig(state string) (*gatewayConfig, error) {
	b, err := os.ReadFile(filepath.Join(state, "gateway.json"))
	if os.IsNotExist(err) {
		if caddySHA256 == "" {
			return nil, nil
		} // Unpackaged development launcher.
		cfg := gatewayConfig{Port: 443, HTTPPort: 80, Addresses: []string{wildcardAddress}}
		if !canBind(wildcardAddress) {
			cfg.Port, cfg.HTTPPort = 8443, 8080
		}
		if err = saveJSON(filepath.Join(state, "gateway.json"), cfg); err != nil {
			return nil, err
		}
		b, err = json.Marshal(cfg)
	}
	if err != nil {
		return nil, err
	}
	var cfg gatewayConfig
	if err = decodeGatewayConfig(b, &cfg); err != nil {
		return nil, err
	}
	if err = cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func startGateway(state string, vm machine) (*gateway, error) {
	saved, err := loadGatewayConfig(state)
	if err != nil || saved == nil {
		return nil, err
	}
	cfg := *saved
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	helper := filepath.Join(filepath.Dir(exe), "caddy")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	if caddySHA256 == "" || !fileHashMatches(helper, caddySHA256) {
		return nil, errors.New("Caddy helper missing or checksum mismatch; use a packaged launcher")
	}
	// Check conflict before any CA material can be generated. Caddy's bind remains
	// authoritative; a subsequent bind race fails instead of choosing another port.
	for _, port := range []int{cfg.Port, cfg.HTTPPort} {
		if port == 0 {
			continue
		}
		for _, address := range cfg.listeners(port) {
			probe, e := net.Listen("tcp4", address)
			if e != nil {
				return nil, fmt.Errorf("gateway bind %s: %w; configure available ports or OS socket privileges without running the VM as root", address, e)
			}
			probe.Close()
		}
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &gateway{config: cfg, cancel: cancel, done: make(chan struct{}), exited: make(chan struct{}), state: state}
	defer func() {
		if err != nil {
			cancel()
			l.Close()
			if g.input != nil {
				g.input.Close()
			}
			os.RemoveAll(g.runDir)
			if g.log != nil {
				g.log.Close()
			}
		}
	}()
	g.runDir, err = os.MkdirTemp(state, "gateway-run-")
	if err != nil {
		return nil, err
	}
	token := randomToken()
	transport := &http.Transport{MaxConnsPerHost: 128, MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second, ResponseHeaderTimeout: 60 * time.Second, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialGateway(ctx, vm)
	}}
	proxy := &httputil.ReverseProxy{Transport: transport, Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(&url.URL{Scheme: "http", Host: "gateway"})
		r.Out.Host = r.In.Host
		// Rewrite removes untrusted forwarding headers. Copy only Caddy's values
		// after its per-boot capability has been checked by this listener.
		for _, key := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
			r.Out.Header[key] = r.In.Header.Values(key)
		}
		r.Out.Header.Del("X-CA-Gateway")
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Workload gateway unavailable", http.StatusBadGateway)
	}}
	g.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: g.bridgeHandler(token, l.Addr().String(), proxy)}
	storage := filepath.Join(state, "gateway", "caddy")
	if err = privateGatewayDir(filepath.Join(state, "gateway")); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(storage, 0700); err != nil {
		return nil, err
	}
	if err = checkGatewayIdentity(storage); err != nil {
		return nil, err
	}
	configFile := filepath.Join(g.runDir, "caddy.json")
	if err = saveJSON(configFile, caddyConfig(cfg, storage, l.Addr().String(), token)); err != nil {
		return nil, err
	}
	g.log, err = os.OpenFile(filepath.Join(state, "gateway.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	g.child = exec.Command(exe, "gateway-helper", "--state", state, "--file", configFile)
	g.child.Env = append(os.Environ(), "XDG_DATA_HOME="+filepath.Join(state, "gateway"), "XDG_CONFIG_HOME="+g.runDir)
	g.child.Stdout, g.child.Stderr = g.log, g.log
	if err = prepareGatewayProcess(g.child); err != nil {
		return nil, err
	}
	g.input, err = g.child.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err = g.child.Start(); err != nil {
		return nil, err
	}
	go func() {
		err := g.child.Wait()
		g.mu.Lock()
		g.childError = fmt.Sprintf("Caddy exited (%v); restart the cluster and inspect gateway.log", err)
		g.mu.Unlock()
		close(g.exited)
	}()
	go g.server.Serve(l)
	go func() {
		defer close(g.done)
		defer g.stopDiscovery()
		defer transport.CloseIdleConnections()
		poll := &http.Transport{DialContext: vm.Dial}
		defer poll.CloseIdleConnections()
		client := &http.Client{Transport: poll, Timeout: 10 * time.Second}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			g.refresh(ctx, client)
			g.refreshDiscovery(ctx)
			g.reportPublications(ctx, client)
			select {
			case <-ctx.Done():
				return
			case <-g.exited:
				return
			case <-ticker.C:
			}
		}
	}()
	return g, nil
}

func (g *gateway) bridgeHandler(token, address string, proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+token+"/ask" && r.Method == "GET" && r.Host == address {
			if record, ok := g.record(r.URL.Query().Get("domain")); ok && record.TLS.Mode == "LocalCA" {
				w.WriteHeader(200)
			} else {
				w.WriteHeader(403)
			}
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		record, ok := g.record(host)
		if r.Header.Get("X-CA-Gateway") != token || !ok || r.Method == "CONNECT" {
			http.Error(w, "Unpublished hostname", 403)
			return
		}
		if !record.LAN {
			ip := net.ParseIP(r.Header.Get("X-Forwarded-For"))
			if ip == nil || !ip.IsLoopback() {
				http.Error(w, "Publication is loopback-only", 403)
				return
			}
		}
		if record.TLS.Mode == "HTTP" && r.Header.Get("X-Forwarded-Proto") != "http" {
			http.Error(w, "HTTPS is disabled for this publication", 403)
			return
		}
		if r.URL.Path == "/root.crt" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/x-x509-ca-cert")
			http.ServeFile(w, r, filepath.Join(g.state, "gateway", "caddy", "pki", "authorities", "local", "root.crt"))
			return
		}
		if r.Header.Get("X-Forwarded-Proto") == "http" && record.TLS.Mode == "LocalCA" {
			authority := host
			if g.config.Port != 443 {
				authority = net.JoinHostPort(host, fmt.Sprint(g.config.Port))
			}
			http.Redirect(w, r, "https://"+authority+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		r.Header.Set("X-Forwarded-Host", r.Host)
		proxy.ServeHTTP(w, r)
	})
}

func (g *gateway) refresh(ctx context.Context, client *http.Client) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://guest/_agent/gateway/publications", nil)
	resp, err := client.Do(req)
	var snapshot publication.Snapshot
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			err = fmt.Errorf("gateway discovery unavailable (%d)", resp.StatusCode)
		} else {
			decoder := json.NewDecoder(io.LimitReader(resp.Body, (1<<20)+1))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&snapshot)
			if err == nil {
				if decoder.Decode(new(any)) != io.EOF {
					err = errors.New("invalid publication snapshot tail")
				} else {
					err = snapshot.Validate()
				}
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.err = "Publication discovery unavailable; retaining last approved hosts"
		return
	}
	publication.Sort(snapshot.Items)
	if !slices.Equal(snapshot.Items, g.publications) {
		if g.advertiser != nil {
			g.advertiser.Close()
			g.advertiser = nil
		}
		g.endpoints = nil
	}
	g.publications = snapshot.Items
	g.hosts = nil
	for _, record := range snapshot.Items {
		g.hosts = append(g.hosts, record.Hostname)
	}
	g.err = ""
}

func (g *gateway) reportPublications(ctx context.Context, client *http.Client) {
	g.mu.RLock()
	feedback := []publication.Feedback{}
	if g.err == "" {
		for _, record := range g.publications {
			f := publication.Feedback{Record: record, URL: g.endpoint(record).url(), DiscoveryError: g.discoveryError}
			for _, e := range g.endpoints {
				if e.URL == f.URL {
					f.Ready, f.CertificateReady, f.Advertised, f.Error = e.Ready, e.CertificateReady, e.Advertised, e.Error
				}
			}
			feedback = append(feedback, f)
		}
	}
	g.mu.RUnlock()
	b, _ := json.Marshal(feedback)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://guest/_agent/gateway/publication-status", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			err = fmt.Errorf("status feedback HTTP %d", resp.StatusCode)
		}
	}
	if err != nil {
		g.mu.Lock()
		g.err = "Publication status feedback unavailable: " + err.Error()
		g.mu.Unlock()
	}
}
func (g *gateway) Close() {
	if g == nil {
		return
	}
	g.cancel()
	<-g.done
	g.mu.Lock()
	if g.advertiser != nil {
		g.advertiser.Close()
		g.advertiser = nil
	}
	g.mu.Unlock()
	// The supervisor stops/reaps Caddy on stdin EOF, including launcher crashes.
	// It holds a separate backup exclusion lock until Caddy has actually exited.
	g.input.Close()
	<-g.exited
	g.server.Close()
	g.log.Close()
	os.RemoveAll(g.runDir)
}

func runGatewayHelper(state, configFile string) error {
	lockDir := filepath.Join(state, "gateway-lock")
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return err
	}
	unlock, err := lockState(lockDir)
	if err != nil {
		return err
	}
	defer unlock()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	helper := filepath.Join(filepath.Dir(exe), "caddy")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	if caddySHA256 == "" || !fileHashMatches(helper, caddySHA256) {
		return errors.New("Caddy checksum mismatch")
	}
	cmd := exec.Command(helper, "run", "--config", configFile)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err = prepareGatewayProcess(cmd); err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	closed := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(closed) }()
	select {
	case err := <-exited:
		return err
	case <-closed:
		if err := cmd.Process.Signal(os.Interrupt); err == nil {
			select {
			case <-exited:
				return nil
			case <-time.After(15 * time.Second):
			}
		}
		_ = cmd.Process.Kill()
		<-exited
		return nil
	}
}

func checkGatewayIdentity(storage string) error {
	root := filepath.Join(storage, "pki", "authorities", "local")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	for _, name := range []string{"root.crt", "root.key"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return errors.New("Caddy CA identity is incomplete; restore gateway/caddy from backup instead of generating replacement trust")
		}
	}
	return nil
}

type bufferedGatewayConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedGatewayConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func dialGateway(ctx context.Context, vm machine) (net.Conn, error) {
	c, err := vm.Dial(ctx, "tcp", "gateway")
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if _, err = io.WriteString(c, "CONNECT /_agent/gateway/connect HTTP/1.1\r\nHost: guest\r\n\r\n"); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: "CONNECT"})
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		c.Close()
		return nil, fmt.Errorf("gateway connect: %s", resp.Status)
	}
	c.SetDeadline(time.Time{})
	return &bufferedGatewayConn{Conn: c, reader: r}, nil
}

func caddyConfig(c gatewayConfig, storage, upstream, token string) map[string]any {
	config := map[string]any{
		"admin":   map[string]any{"disabled": true},
		"storage": map[string]any{"module": "file_system", "root": storage},
		"apps": map[string]any{
			"pki": map[string]any{"certificate_authorities": map[string]any{"local": map[string]any{"name": "Containers Anywhere", "install_trust": false}}},
			"tls": map[string]any{"automation": map[string]any{
				"on_demand": map[string]any{"ask": "http://" + upstream + "/" + token + "/ask"},
				"policies":  []any{map[string]any{"on_demand": true, "issuers": []any{map[string]any{"module": "internal"}}}},
			}},
			"http": map[string]any{"servers": map[string]any{"workloads": map[string]any{
				"listen":                  c.listeners(c.Port),
				"protocols":               []string{"h1", "h2"},
				"strict_sni_host":         true,
				"automatic_https":         map[string]any{"disable_redirects": true},
				"tls_connection_policies": []any{map[string]any{}},
				"routes": []any{map[string]any{"handle": []any{map[string]any{
					"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": upstream}},
					"headers":            map[string]any{"request": map[string]any{"set": map[string]any{"X-CA-Gateway": []string{token}}, "delete": []string{"Forwarded", "X-Real-IP"}}},
					"stream_close_delay": "5m",
				}}}},
			}}},
		},
	}
	servers := config["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)
	if c.HTTPPort != 0 {
		servers["bootstrap"] = map[string]any{"listen": c.listeners(c.HTTPPort), "protocols": []string{"h1"}, "automatic_https": map[string]any{"disable": true}, "routes": servers["workloads"].(map[string]any)["routes"]}
	}
	return config
}

const wildcardAddress = "0.0.0.0"

// Occupied ports fail startup explicitly; only missing low-port permission
// selects high ports. Wildcard binding also supports macOS's low-port policy.
func canBind(address string) bool {
	for _, port := range []int{443, 80} {
		l, err := net.Listen("tcp4", net.JoinHostPort(address, fmt.Sprint(port)))
		if errors.Is(err, os.ErrPermission) {
			return false
		}
		if l != nil {
			l.Close()
		}
	}
	return true
}
