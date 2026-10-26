package main

// Interface filtering, per-link multicast sockets and conflict-before-advertise
// follow CARE Clinic's app/internal/sys/mdns (MIT, OHC Network Foundation 2026).
// Advertisements follow approved, responding host->Caddy->VM->Traefik routes.
import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

type demoLink struct {
	iface   net.Interface
	ip      net.IP
	network *net.IPNet
}

func demoLinks(allowed []string) ([]demoLink, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var links []demoLink
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		virtual := false
		for _, prefix := range []string{"docker", "br-", "veth", "virbr", "vboxnet", "vmnet", "utun", "tun", "tap", "vethernet"} {
			if strings.HasPrefix(strings.ToLower(iface.Name), prefix) {
				virtual = true
			}
		}
		if virtual {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok || network.IP.To4() == nil || !network.IP.IsPrivate() {
				continue
			}
			selected := allowed == nil
			for _, ip := range allowed {
				if network.IP.Equal(net.ParseIP(ip)) {
					selected = true
				}
			}
			if selected {
				links = append(links, demoLink{iface, network.IP.To4(), network})
				break
			}
		}
	}
	return links, nil
}

var demoGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
var demoName = dnsmessage.MustNewName("demo.local.")
var demoService = dnsmessage.MustNewName("_http._tcp.local.")
var demoInstance = dnsmessage.MustNewName("Containers Anywhere._http._tcp.local.")

type mdnsEndpoint struct {
	Host string
	Port uint16
	HTTP bool
}

func (e mdnsEndpoint) names() (dnsmessage.Name, dnsmessage.Name, dnsmessage.Name) {
	name := dnsmessage.MustNewName(e.Host + ".")
	if e.Host == "demo.local" && e.HTTP {
		return name, demoService, demoInstance
	}
	service := "_https._tcp.local."
	if e.HTTP {
		service = "_http._tcp.local."
	}
	id := sha256.Sum256([]byte(e.Host))
	return name, dnsmessage.MustNewName(service), dnsmessage.MustNewName(fmt.Sprintf("Containers Anywhere %x.%s", id[:8], service))
}

type demoSocket struct {
	conn *ipv4.PacketConn
	link demoLink
}
type demoAdvertiser struct {
	sockets   []demoSocket
	done      chan struct{}
	wg        sync.WaitGroup
	once      sync.Once
	endpoints []mdnsEndpoint
	mu        sync.Mutex
	err       error
}

func demoRecords(ip net.IP, port uint16, ttl uint32) []dnsmessage.Resource {
	return (mdnsEndpoint{Host: "demo.local", Port: port, HTTP: true}).records(ip, ttl)
}

func (e mdnsEndpoint) records(ip net.IP, ttl uint32) []dnsmessage.Resource {
	name, service, instance := e.names()
	var addr [4]byte
	copy(addr[:], ip.To4())
	header := func(name dnsmessage.Name, t dnsmessage.Type, unique bool) dnsmessage.ResourceHeader {
		class := dnsmessage.ClassINET
		if unique {
			class |= 0x8000
		}
		return dnsmessage.ResourceHeader{Name: name, Type: t, Class: class, TTL: ttl}
	}
	return []dnsmessage.Resource{
		{Header: header(name, dnsmessage.TypeA, true), Body: &dnsmessage.AResource{A: addr}},
		{Header: header(service, dnsmessage.TypePTR, false), Body: &dnsmessage.PTRResource{PTR: instance}},
		{Header: header(instance, dnsmessage.TypeSRV, true), Body: &dnsmessage.SRVResource{Port: e.Port, Target: name}},
		{Header: header(instance, dnsmessage.TypeTXT, true), Body: &dnsmessage.TXTResource{TXT: []string{"path=/", "product=containers-anywhere"}}},
	}
}

func demoConflict(m dnsmessage.Message, own net.IP) bool {
	return (mdnsEndpoint{Host: "demo.local"}).conflict(m, own)
}

func (e mdnsEndpoint) conflict(m dnsmessage.Message, own net.IP) bool {
	for _, rr := range append(append(m.Answers, m.Authorities...), m.Additionals...) {
		if !strings.EqualFold(rr.Header.Name.String(), e.Host+".") {
			continue
		}
		if a, ok := rr.Body.(*dnsmessage.AResource); ok && !net.IP(a.A[:]).Equal(own) && rr.Header.TTL > 0 {
			return true
		}
	}
	return false
}

func startWorkloadAdvertiser(ctx context.Context, links []demoLink, endpoints []mdnsEndpoint) (*demoAdvertiser, error) {
	a := &demoAdvertiser{done: make(chan struct{}), endpoints: slices.Clone(endpoints)}
	fail := func(err error) (*demoAdvertiser, error) {
		for _, s := range a.sockets {
			s.conn.Close()
		}
		return nil, err
	}
	for _, link := range links {
		udp, err := net.ListenMulticastUDP("udp4", &link.iface, demoGroup)
		if err != nil {
			return fail(err)
		}
		conn := ipv4.NewPacketConn(udp)
		a.sockets = append(a.sockets, demoSocket{conn, link})
		for _, err := range []error{conn.SetMulticastInterface(&link.iface), conn.SetTTL(255), conn.SetMulticastTTL(255), conn.SetMulticastLoopback(true)} {
			if err != nil {
				return fail(err)
			}
		}
		// Three probes, including proposed address, before claiming the shared name.
		if err := conn.SetControlMessage(ipv4.FlagInterface, true); err != nil && runtime.GOOS != "windows" {
			return fail(err)
		}
		buf := make([]byte, 9000)
		for i := 0; i < 3; i++ {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			for _, endpoint := range a.endpoints {
				name, _, _ := endpoint.names()
				query := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET}}, Authorities: endpoint.records(link.ip, 120)[:1]}
				wire, packErr := query.Pack()
				if packErr != nil {
					return fail(packErr)
				}
				conn.SetWriteDeadline(time.Now().Add(time.Second))
				if _, err = conn.WriteTo(wire, nil, demoGroup); err != nil {
					return fail(err)
				}
			}
			conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			for {
				n, control, source, err := conn.ReadFrom(buf)
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						break
					}
					return fail(err)
				}
				from, ok := source.(*net.UDPAddr)
				if control != nil && control.IfIndex != 0 && control.IfIndex != link.iface.Index {
					continue
				}
				if !ok || from.Port != 5353 || !link.network.Contains(from.IP) {
					continue
				}
				var response dnsmessage.Message
				if response.Unpack(buf[:n]) == nil {
					for _, endpoint := range a.endpoints {
						if endpoint.conflict(response, link.ip) {
							return fail(fmt.Errorf("%s is already in use on %s; advertisement withheld", endpoint.Host, link.iface.Name))
						}
					}
				}
			}
		}
		conn.SetReadDeadline(time.Time{})
	}
	a.announce(120)
	if a.err != nil {
		a.announce(0)
		return fail(a.err)
	}
	for _, socket := range a.sockets {
		a.wg.Add(1)
		go a.serve(socket)
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-a.done:
				return
			case <-ticker.C:
				a.announce(120)
			}
		}
	}()
	return a, nil
}

func (a *demoAdvertiser) send(s demoSocket, m dnsmessage.Message, to net.Addr) {
	wire, err := m.Pack()
	if err == nil {
		s.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_, err = s.conn.WriteTo(wire, nil, to)
	}
	if err != nil {
		a.mu.Lock()
		a.err = err
		a.mu.Unlock()
	}
}
func (a *demoAdvertiser) announce(ttl uint32) {
	a.mu.Lock()
	failed := a.err != nil
	a.mu.Unlock()
	if failed && ttl != 0 {
		return
	}
	for _, s := range a.sockets {
		for _, endpoint := range a.endpoints {
			a.send(s, dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}, Answers: endpoint.records(s.link.ip, ttl)}, demoGroup)
		}
	}
}
func (a *demoAdvertiser) serve(s demoSocket) {
	defer a.wg.Done()
	buf := make([]byte, 9000)
	for {
		n, control, source, err := s.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-a.done:
			default:
				a.mu.Lock()
				a.err = err
				a.mu.Unlock()
			}
			return
		}
		from, ok := source.(*net.UDPAddr)
		if control != nil && control.IfIndex != 0 && control.IfIndex != s.link.iface.Index {
			continue
		}
		if !ok || !s.link.network.Contains(from.IP) {
			continue
		}
		var query dnsmessage.Message
		if query.Unpack(buf[:n]) != nil {
			continue
		}
		for _, endpoint := range a.endpoints {
			if endpoint.conflict(query, s.link.ip) {
				a.mu.Lock()
				a.err = fmt.Errorf("%s name conflict detected; advertisement withdrawn", endpoint.Host)
				a.mu.Unlock()
				return
			}
		}
		if query.Response {
			continue
		}
		a.mu.Lock()
		failed := a.err != nil
		a.mu.Unlock()
		if failed {
			continue
		}
		for _, q := range query.Questions {
			if q.Class&0x7fff != dnsmessage.ClassINET {
				continue
			}
			for _, endpoint := range a.endpoints {
				var answers []dnsmessage.Resource
				for _, rr := range endpoint.records(s.link.ip, 120) {
					if strings.EqualFold(q.Name.String(), rr.Header.Name.String()) && (q.Type == rr.Header.Type || q.Type == dnsmessage.TypeALL) {
						known := false
						for _, answer := range query.Answers {
							if answer.Header.Name == rr.Header.Name && answer.Header.Type == rr.Header.Type && answer.Header.TTL >= 60 && fmt.Sprint(answer.Body) == fmt.Sprint(rr.Body) {
								known = true
							}
						}
						if known && from.Port == 5353 && q.Class&0x8000 == 0 {
							continue
						}
						answers = append(answers, rr)
					}
				}
				if len(answers) == 0 {
					continue
				}
				response := dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}, Answers: answers}
				if q.Type == dnsmessage.TypePTR {
					response.Additionals = endpoint.records(s.link.ip, 120)
				}
				var to net.Addr = demoGroup
				if from.Port != 5353 {
					to = from
					response.ID = query.ID
					response.Questions = query.Questions
					for i := range response.Answers {
						response.Answers[i].Header.Class = dnsmessage.ClassINET
						response.Answers[i].Header.TTL = 10
					}
				} else if q.Class&0x8000 != 0 {
					to = from
				}
				a.send(s, response, to)
			}
		}
	}
}
func (a *demoAdvertiser) Close() {
	a.once.Do(func() {
		close(a.done)
		for _, s := range a.sockets {
			s.conn.SetReadDeadline(time.Now())
		}
		a.wg.Wait()
		a.announce(0)
		for _, s := range a.sockets {
			s.conn.Close()
		}
	})
}

func (g *gateway) refreshDiscovery(ctx context.Context) {
	g.mu.RLock()
	hosts := slices.Clone(g.hosts)
	processError := g.childError
	g.mu.RUnlock()
	allowed := g.config.Addresses
	if slices.Contains(allowed, wildcardAddress) {
		allowed = nil
	}
	var links []demoLink
	var discoveryErr error
	if g.config.MDNS {
		links, discoveryErr = demoLinks(allowed)
		if discoveryErr == nil && len(links) == 0 {
			discoveryErr = errors.New("no configured LAN interface is available")
		}
	}
	addresses := g.config.Addresses
	if len(addresses) == 0 || slices.Contains(addresses, wildcardAddress) {
		addresses = []string{"127.0.0.1"}
		for _, link := range links {
			addresses = append(addresses, link.ip.String())
		}
	}
	// A DNS answer or Caddy's HTTP->HTTPS redirect does not establish that the
	// application is serving. Probe HTTPS with SNI and the persisted private CA.
	ca, caErr := os.ReadFile(filepath.Join(g.state, "gateway/caddy/pki/authorities/local/root.crt"))
	roots := x509.NewCertPool()
	if caErr == nil && !roots.AppendCertsFromPEM(ca) {
		caErr = errors.New("invalid gateway root certificate")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var endpoints []gatewayEndpoint
	var advertised []mdnsEndpoint
	for _, host := range hosts {
		endpoint := mdnsEndpoint{Host: host, Port: uint16(g.config.Port)}
		if (host == "demo.local" || host == "demo.localhost") && g.config.HTTPPort != 0 {
			endpoint.HTTP, endpoint.Port = true, uint16(g.config.HTTPPort)
		}
		status := gatewayEndpoint{URL: endpoint.url()}
		var err error
		switch {
		case processError != "":
			err = errors.New(processError)
		case !endpoint.HTTP && caErr != nil:
			err = fmt.Errorf("gateway CA unavailable: %w", caErr)
		default:
			for _, address := range addresses {
				if err = probeWorkload(probeCtx, endpoint, address, roots); err != nil {
					break
				}
			}
		}
		status.Ready = err == nil
		if err != nil {
			status.Error = err.Error()
		} else if g.config.MDNS && strings.HasSuffix(host, ".local") {
			advertised = append(advertised, endpoint)
		}
		endpoints = append(endpoints, status)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.endpoints = endpoints
	if g.advertiser != nil {
		g.advertiser.mu.Lock()
		advErr := g.advertiser.err
		g.advertiser.mu.Unlock()
		if advErr != nil {
			discoveryErr = advErr
		}
		if len(links) != len(g.advertiser.sockets) {
			discoveryErr = errors.New("LAN interfaces changed; reconfigure gateway addresses")
		} else {
			for i, link := range links {
				old := g.advertiser.sockets[i].link
				if link.iface.Index != old.iface.Index || !link.ip.Equal(old.ip) {
					discoveryErr = errors.New("LAN addresses changed; reconfigure gateway addresses")
				}
			}
		}
		if discoveryErr != nil || !slices.Equal(advertised, g.advertiser.endpoints) {
			g.advertiser.Close()
			g.advertiser = nil
		}
	}
	if discoveryErr == nil && len(advertised) > 0 && g.advertiser == nil {
		g.advertiser, discoveryErr = startWorkloadAdvertiser(ctx, links, advertised)
	}
	g.discoveryError = ""
	if discoveryErr != nil {
		g.discoveryError = discoveryErr.Error()
	}
	if g.advertiser != nil {
		for i := range g.endpoints {
			for _, endpoint := range advertised {
				if g.endpoints[i].URL == endpoint.url() {
					g.endpoints[i].Advertised = true
				}
			}
		}
	}
}

func (e mdnsEndpoint) url() string {
	scheme := "https"
	if e.HTTP {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d/", scheme, e.Host, e.Port)
}

func probeWorkload(ctx context.Context, endpoint mdnsEndpoint, address string, roots *x509.CertPool) error {
	transport := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort(address, fmt.Sprint(endpoint.Port)))
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.url(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("workload probe on %s: %w", address, err)
	}
	defer resp.Body.Close()
	if endpoint.HTTP {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32768))
		if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "Containers Anywhere works.") {
			return fmt.Errorf("demo not serving on %s (HTTP %d)", address, resp.StatusCode)
		}
	} else if !(resp.StatusCode >= 200 && resp.StatusCode < 400 || resp.StatusCode == 401 || resp.StatusCode == 403) {
		return fmt.Errorf("workload root not serving on %s (HTTP %d)", address, resp.StatusCode)
	}
	return nil
}

func (g *gateway) stopDiscovery() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.advertiser != nil {
		g.advertiser.Close()
		g.advertiser = nil
	}
	for i := range g.endpoints {
		g.endpoints[i].Ready = false
		g.endpoints[i].Advertised = false
		g.endpoints[i].Error = "gateway stopped"
	}
}
