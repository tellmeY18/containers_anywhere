package main

// Interface filtering, per-link multicast sockets and conflict-before-advertise
// follow CARE Clinic's app/internal/sys/mdns (MIT, OHC Network Foundation 2026).
// This implementation uses the existing x/net DNS codec and advertises only the
// installed demo after checking the actual host->Caddy->VM->Traefik->pod path.
import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
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

// Names for the preinstalled CNCM manager and the CARE deployment it creates.
// They resolve to the same address as demo.local and are routed by the gateway.
var extraNames = []dnsmessage.Name{
	dnsmessage.MustNewName("cncm.cncm-system.local."),
	dnsmessage.MustNewName("care.care.local."),
}

type demoSocket struct {
	conn *ipv4.PacketConn
	link demoLink
}
type demoAdvertiser struct {
	sockets []demoSocket
	done    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
	port    uint16
	mu      sync.Mutex
	err     error
}

func demoRecords(ip net.IP, port uint16, ttl uint32) []dnsmessage.Resource {
	var addr [4]byte
	copy(addr[:], ip.To4())
	header := func(name dnsmessage.Name, t dnsmessage.Type, unique bool) dnsmessage.ResourceHeader {
		class := dnsmessage.ClassINET
		if unique {
			class |= 0x8000
		}
		return dnsmessage.ResourceHeader{Name: name, Type: t, Class: class, TTL: ttl}
	}
	records := []dnsmessage.Resource{
		{Header: header(demoName, dnsmessage.TypeA, true), Body: &dnsmessage.AResource{A: addr}},
		{Header: header(demoService, dnsmessage.TypePTR, false), Body: &dnsmessage.PTRResource{PTR: demoInstance}},
		{Header: header(demoInstance, dnsmessage.TypeSRV, true), Body: &dnsmessage.SRVResource{Port: port, Target: demoName}},
		{Header: header(demoInstance, dnsmessage.TypeTXT, true), Body: &dnsmessage.TXTResource{TXT: []string{"path=/", "product=containers-anywhere"}}},
	}
	for _, name := range extraNames {
		records = append(records, dnsmessage.Resource{Header: header(name, dnsmessage.TypeA, true), Body: &dnsmessage.AResource{A: addr}})
	}
	return records
}

func demoConflict(m dnsmessage.Message, own net.IP) bool {
	for _, rr := range append(append(m.Answers, m.Authorities...), m.Additionals...) {
		if !strings.EqualFold(rr.Header.Name.String(), demoName.String()) {
			continue
		}
		if a, ok := rr.Body.(*dnsmessage.AResource); ok && !net.IP(a.A[:]).Equal(own) && rr.Header.TTL > 0 {
			return true
		}
	}
	return false
}

func startDemoAdvertiser(ctx context.Context, links []demoLink, port int) (*demoAdvertiser, error) {
	a := &demoAdvertiser{done: make(chan struct{}), port: uint16(port)}
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
		query := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: demoName, Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET}}, Authorities: demoRecords(link.ip, a.port, 120)[:1]}
		wire, _ := query.Pack()
		buf := make([]byte, 9000)
		for i := 0; i < 3; i++ {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err = conn.WriteTo(wire, nil, demoGroup); err != nil {
				return fail(err)
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
				if response.Unpack(buf[:n]) == nil && demoConflict(response, link.ip) {
					return fail(fmt.Errorf("demo.local is already in use on %s; stop the other demo before publishing", link.iface.Name))
				}
			}
		}
		conn.SetReadDeadline(time.Time{})
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
		a.announce(120)
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
		a.send(s, dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}, Answers: demoRecords(s.link.ip, a.port, ttl)}, demoGroup)
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
		if demoConflict(query, s.link.ip) {
			a.mu.Lock()
			a.err = errors.New("demo.local name conflict detected; advertisement withdrawn")
			a.mu.Unlock()
			return
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
			var answers []dnsmessage.Resource
			for _, rr := range demoRecords(s.link.ip, a.port, 120) {
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
				response.Additionals = demoRecords(s.link.ip, a.port, 120)
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
	if !g.config.MDNS {
		return
	}
	// A real request must traverse both proxies and the guest before announcing.
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, "GET", fmt.Sprintf("http://127.0.0.1:%d/", g.config.HTTPPort), nil)
	req.Host = "demo.local"
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err == nil {
		b, e := io.ReadAll(io.LimitReader(resp.Body, 32768))
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 || !strings.Contains(string(b), "Containers Anywhere works.") {
			err = errors.New("demo workload not ready")
		}
	}
	links, linkErr := demoLinks(g.config.Addresses)
	if err == nil {
		err = linkErr
	}
	if err == nil && len(links) == 0 {
		err = errors.New("no configured LAN interface is available")
	}
	// Check the bound LAN addresses as well as loopback. This catches a failed
	// host listener, but only a second-device check can prove firewall passage.
	if err == nil {
		for _, link := range links {
			probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			r, _ := http.NewRequestWithContext(probeCtx, "GET", fmt.Sprintf("http://%s:%d/", link.ip, g.config.HTTPPort), nil)
			r.Host = "demo.local"
			response, e := client.Do(r)
			if e == nil {
				body, readErr := io.ReadAll(io.LimitReader(response.Body, 32768))
				response.Body.Close()
				if readErr != nil || response.StatusCode != 200 || !strings.Contains(string(body), "Containers Anywhere works.") {
					e = errors.New("LAN demo listener is not serving the workload")
				}
			}
			cancel()
			if e != nil {
				err = e
				break
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.advertiser != nil {
		g.advertiser.mu.Lock()
		advErr := g.advertiser.err
		g.advertiser.mu.Unlock()
		if advErr != nil {
			err = advErr
		}
		if len(links) != len(g.advertiser.sockets) {
			err = errors.New("LAN interfaces changed; reconfigure gateway addresses")
		} else {
			for i, link := range links {
				old := g.advertiser.sockets[i].link
				if link.iface.Index != old.iface.Index || !link.ip.Equal(old.ip) {
					err = errors.New("LAN addresses changed; reconfigure gateway addresses")
				}
			}
		}
	}
	if err != nil {
		if g.advertiser != nil {
			g.advertiser.Close()
			g.advertiser = nil
		}
		g.discoveryError = err.Error()
		return
	}
	if g.advertiser == nil {
		g.advertiser, err = startDemoAdvertiser(ctx, links, g.config.HTTPPort)
		if err != nil {
			g.discoveryError = err.Error()
			return
		}
	}
	g.discoveryError = ""
}
