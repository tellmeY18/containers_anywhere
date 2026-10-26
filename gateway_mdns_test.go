package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/publication"
	"golang.org/x/net/dns/dnsmessage"
)

func TestDemoMDNSRecordsAndConflict(t *testing.T) {
	ip := net.IPv4(192, 168, 42, 10)
	m := dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}, Answers: demoRecords(ip, 443, 120)}
	wire, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	var parsed dnsmessage.Message
	if err = parsed.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Answers) != 4 {
		t.Fatal(parsed)
	}
	if demoConflict(parsed, ip) {
		t.Fatal("own address is a conflict")
	}
	if !demoConflict(parsed, net.IPv4(192, 168, 42, 11)) {
		t.Fatal("missed another appliance")
	}
	for _, rr := range parsed.Answers {
		if rr.Header.Type == dnsmessage.TypeAAAA {
			t.Fatal("advertised unserved IPv6")
		}
		if srv, ok := rr.Body.(*dnsmessage.SRVResource); ok && (srv.Port != 443 || srv.Target != demoName || rr.Header.Name != demoInstance) {
			t.Fatal(srv)
		}
	}
	goodbye := dnsmessage.Message{Answers: demoRecords(ip, 443, 0)}
	if demoConflict(goodbye, net.IPv4(192, 168, 42, 11)) {
		t.Fatal("goodbye blocks name reuse")
	}
}

func TestWorkloadMDNSNamesUseApprovedEndpointAndHTTPSPort(t *testing.T) {
	ip := net.IPv4(192, 168, 42, 10)
	for _, host := range []string{"app.example.local", "cncm.cncm-system.local", "care.care.local"} {
		endpoint := mdnsEndpoint{Host: host, Port: 8443}
		m := dnsmessage.Message{Answers: endpoint.records(ip, 120)}
		wire, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		var parsed dnsmessage.Message
		if err := parsed.Unpack(wire); err != nil {
			t.Fatal(err)
		}
		if len(parsed.Answers) != 4 || endpoint.conflict(parsed, ip) || !endpoint.conflict(parsed, net.IPv4(192, 168, 42, 11)) {
			t.Fatal("incorrect records or conflict detection", parsed)
		}
		for _, record := range parsed.Answers {
			switch body := record.Body.(type) {
			case *dnsmessage.AResource:
				if record.Header.Name.String() != host+"." {
					t.Fatal("advertised an unrelated application", record)
				}
			case *dnsmessage.PTRResource:
				if record.Header.Name.String() != "_https._tcp.local." {
					t.Fatal("wrong service protocol")
				}
			case *dnsmessage.SRVResource:
				if body.Target.String() != host+"." || body.Port != 8443 {
					t.Fatal("wrong endpoint", body)
				}
			}
		}
		if endpoint.conflict(dnsmessage.Message{Answers: endpoint.records(ip, 0)}, net.IPv4(192, 168, 42, 11)) {
			t.Fatal("goodbye blocks name reuse")
		}
	}
}

func TestWorkloadProbeVerifiesTLSAndDoesNotFollowRedirects(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed application redirect")
	}))
	defer redirected.Close()
	var expectedHost string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != expectedHost || r.Method != "GET" || r.URL.Path != "/" {
			t.Errorf("incorrect probe: %s %s host=%s", r.Method, r.URL, r.Host)
		}
		w.Header().Set("Location", redirected.URL)
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	endpoint := mdnsEndpoint{Host: server.Certificate().DNSNames[0], Port: uint16(n)}
	expectedHost = net.JoinHostPort(endpoint.Host, port)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	for _, code := range []int{200, 204, 302, 401, 403, 404, 500, 502, 503} {
		status.Store(int32(code))
		cert, err := probeWorkload(context.Background(), endpoint, host, roots)
		if !cert {
			t.Fatal("valid certificate not reported independently of workload response")
		}
		if (err == nil) != (code < 400 || code == 401 || code == 403) {
			t.Fatalf("HTTP %d: %v", code, err)
		}
	}
	status.Store(200)
	if _, err := probeWorkload(context.Background(), endpoint, host, x509.NewCertPool()); err == nil {
		t.Fatal("accepted untrusted TLS")
	}
	endpoint.Host = "wrong.example.local"
	if _, err := probeWorkload(context.Background(), endpoint, host, roots); err == nil {
		t.Fatal("accepted wrong TLS hostname")
	}
}

func TestDemoProbeRequiresVerifiedHTTPSPageAndSelectsHTTPSDemoURL(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "demo.local"}, DNSNames: []string{"demo.local"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	var page atomic.Value
	page.Store("<h1>Containers Anywhere works.</h1>")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "demo.local" || !strings.HasPrefix(r.Host, "demo.local:") {
			t.Errorf("incorrect SNI/Host: %s %s", r.TLS.ServerName, r.Host)
		}
		io.WriteString(w, page.Load().(string))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	endpoint := mdnsEndpoint{Host: "demo.local", Port: uint16(n)}
	if ready, err := probeWorkload(context.Background(), endpoint, host, roots); err != nil || !ready {
		t.Fatal("demo HTTPS probe failed", ready, err)
	}
	if _, err := probeWorkload(context.Background(), endpoint, host, x509.NewCertPool()); err == nil {
		t.Fatal("accepted demo certificate from an untrusted CA")
	}
	page.Store("some other application")
	if _, err := probeWorkload(context.Background(), endpoint, host, roots); err == nil {
		t.Fatal("accepted a page that is not the packaged demo")
	}
	g := &gateway{endpoints: []gatewayEndpoint{{URL: "http://demo.local:80/", Ready: true}, {URL: "https://demo.local:443/", Ready: true, CertificateReady: true}}}
	if url := g.status()["demoURL"]; url != "https://demo.local:443/" {
		t.Fatal("demoURL is not the HTTPS demo endpoint", url)
	}
	record := publication.Record{Hostname: "demo.local", LAN: true, MDNS: true, TLS: publication.TLS{Mode: "LocalCA"}}
	g.config = gatewayConfig{Port: 443, HTTPPort: 80}
	if url := g.endpoint(record).url(); url != "https://demo.local:443/" {
		t.Fatal(url)
	}
}

func TestDiscoveryDistinguishesServingFromApprovalAndWithdraws(t *testing.T) {
	var code atomic.Int32
	code.Store(200)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(code.Load()))
		io.WriteString(w, "workload")
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	host := server.Certificate().DNSNames[0]
	g := &gateway{state: t.TempDir(), config: gatewayConfig{Port: n}, hosts: []string{host}}
	record := testPublication(host)
	g.publications = []publication.Record{record}
	ca := filepath.Join(g.state, "gateway/caddy/pki/authorities/local/root.crt")
	if err := os.MkdirAll(filepath.Dir(ca), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	g.refreshDiscovery(context.Background())
	if len(g.endpoints) != 1 || !g.endpoints[0].Ready || g.endpoints[0].Advertised {
		t.Fatal("approved responding workload not identified", g.status())
	}
	if urls := g.status()["urls"].([]string); len(urls) != 1 || urls[0] != fmt.Sprintf("https://%s:%d", host, n) {
		t.Fatal("approval URLs changed")
	}
	old := &demoAdvertiser{done: make(chan struct{}), endpoints: []mdnsEndpoint{{Host: host, Port: uint16(n)}}}
	g.advertiser = old
	code.Store(503)
	g.refreshDiscovery(context.Background())
	if g.endpoints[0].Ready || g.endpoints[0].Error == "" || g.advertiser != nil || !g.permits(host) {
		t.Fatal("failure removed approval or retained serving/advertisement", g.status())
	}
	select {
	case <-old.done:
	default:
		t.Fatal("failed endpoint advertisement was not closed")
	}
	g.hosts = nil
	g.publications = nil
	g.refreshDiscovery(context.Background())
	if len(g.endpoints) != 0 {
		t.Fatal("deleted approval still shown")
	}
	g.hosts = []string{host}
	g.publications = []publication.Record{record}
	code.Store(200)
	g.refreshDiscovery(context.Background())
	g.stopDiscovery()
	if g.endpoints[0].Ready || g.endpoints[0].Advertised || g.endpoints[0].Error == "" {
		t.Fatal("stopped gateway still serving")
	}
}

func TestGatewayLANBindingsAreExplicit(t *testing.T) {
	c := gatewayConfig{Port: 443, HTTPPort: 80, Addresses: []string{"127.0.0.1", "192.168.42.10"}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"::", "8.8.8.8", "localhost", "192.168.42.0/24"} {
		c.Addresses = []string{address}
		if c.validate() == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	c.Addresses = []string{"0.0.0.0"}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.Addresses = []string{"0.0.0.0", "127.0.0.1"}
	if c.validate() == nil {
		t.Fatal("wildcard combined with explicit address")
	}
}
