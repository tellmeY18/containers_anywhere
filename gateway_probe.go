package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// Run on a second device: query multicast directly (not hosts/DNS cache), then
// fetch the demo at the discovered address with its original HTTP Host intact.
// An HTTP demo check proves connectivity, not authenticated appliance identity.
func checkLANDemo() error {
	links, err := demoLinks(nil)
	if err != nil {
		return err
	}
	if len(links) == 0 {
		return errors.New("no multicast-capable private LAN IPv4 interface")
	}
	var failures []error
	for _, link := range links {
		if err := checkDemoLink(link); err == nil {
			fmt.Println("PASS: mDNS -> host Caddy -> authenticated VM bridge -> Traefik -> demo pod")
			return nil
		} else {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func checkDemoLink(link demoLink) error {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: link.ip})
	if err != nil {
		return err
	}
	defer udp.Close()
	conn := ipv4.NewPacketConn(udp)
	if err = conn.SetMulticastInterface(&link.iface); err != nil {
		return err
	}
	if err = conn.SetMulticastTTL(255); err != nil {
		return err
	}
	udp.SetDeadline(time.Now().Add(3 * time.Second))
	query := dnsmessage.Message{Questions: []dnsmessage.Question{
		{Name: demoName, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		{Name: demoInstance, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET},
	}}
	wire, err := query.Pack()
	if err != nil {
		return err
	}
	if _, err = udp.WriteToUDP(wire, demoGroup); err != nil {
		return err
	}
	var ip net.IP
	var port uint16
	buf := make([]byte, 9000)
	for ip == nil || port == 0 {
		n, from, err := udp.ReadFromUDP(buf)
		if err != nil {
			return fmt.Errorf("mDNS on %s: %w", link.iface.Name, err)
		}
		if from.Port != 5353 || !link.network.Contains(from.IP) {
			continue
		}
		var msg dnsmessage.Message
		if msg.Unpack(buf[:n]) != nil || !msg.Response || msg.RCode != dnsmessage.RCodeSuccess {
			continue
		}
		for _, rr := range append(msg.Answers, msg.Additionals...) {
			if rr.Header.TTL == 0 {
				continue
			}
			switch body := rr.Body.(type) {
			case *dnsmessage.AResource:
				if rr.Header.Name == demoName && link.network.Contains(net.IP(body.A[:])) {
					ip = net.IP(append([]byte(nil), body.A[:]...))
				}
			case *dnsmessage.SRVResource:
				if rr.Header.Name == demoInstance && body.Target == demoName {
					port = body.Port
				}
			}
		}
	}
	if ip.Equal(link.ip) {
		return errors.New("demo resolves to this computer; run demo-check from a second LAN device")
	}
	address := net.JoinHostPort(ip.String(), fmt.Sprint(port))
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp4", address)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get("http://demo.local/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32768))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Containers Anywhere works.") {
		return fmt.Errorf("demo at %s did not return expected workload page (HTTP %d)", address, resp.StatusCode)
	}
	fmt.Printf("Discovered demo.local at %s on %s\n", address, link.iface.Name)
	return nil
}
