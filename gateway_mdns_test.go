package main

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDemoMDNSRecordsAndConflict(t *testing.T) {
	ip := net.IPv4(192, 168, 42, 10)
	m := dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}, Answers: demoRecords(ip, 80, 120)}
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
		if srv, ok := rr.Body.(*dnsmessage.SRVResource); ok && (srv.Port != 80 || srv.Target != demoName) {
			t.Fatal(srv)
		}
	}
	goodbye := dnsmessage.Message{Answers: demoRecords(ip, 80, 0)}
	if demoConflict(goodbye, net.IPv4(192, 168, 42, 11)) {
		t.Fatal("goodbye blocks name reuse")
	}
}

func TestGatewayLANBindingsAreExplicit(t *testing.T) {
	c := gatewayConfig{Port: 443, HTTPPort: 80, Addresses: []string{"127.0.0.1", "192.168.42.10"}, MDNS: true, Namespaces: []string{"example"}, Domains: []string{"example.local"}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"0.0.0.0", "::", "8.8.8.8", "localhost", "192.168.42.0/24"} {
		c.Addresses = []string{address}
		if c.validate() == nil {
			t.Fatalf("accepted %s", address)
		}
	}
}
