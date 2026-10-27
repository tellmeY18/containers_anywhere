package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (Publication, Ingress, Service) {
	t.Helper()
	p := Publication{APIVersion: Group + "/v1alpha1", Kind: "WorkloadPublication", Metadata: Metadata{Name: "web", Namespace: "example", UID: "publication-1", Generation: 1, ResourceVersion: "1"},
		Spec: Spec{IngressRef: "web", Hostname: "app.example.local", Enabled: true, LAN: true, MDNS: true, TLS: TLS{Mode: "LocalCA"}}}
	var i Ingress
	if err := json.Unmarshal([]byte(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"web","namespace":"example"},"spec":{"ingressClassName":"containers-anywhere","rules":[{"host":"app.example.local","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`), &i); err != nil {
		t.Fatal(err)
	}
	var s Service
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"web","namespace":"example"},"spec":{"type":"ClusterIP","clusterIP":"10.43.0.2","selector":{"app":"web"},"ports":[{"port":80,"protocol":"TCP"}]}}`), &s); err != nil {
		t.Fatal(err)
	}
	return p, i, s
}

func TestBuildRouteBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Publication, *Ingress, *Service)
	}{
		{"other namespace", func(p *Publication, i *Ingress, s *Service) { i.Metadata.Namespace = "other" }},
		{"foreign hostname", func(p *Publication, i *Ingress, s *Service) {
			p.Spec.Hostname = "app.other.local"
			i.Spec.Rules[0].Host = p.Spec.Hostname
		}},
		{"protected namespace", func(p *Publication, i *Ingress, s *Service) { p.Metadata.Namespace = "kite-system" }},
		{"reserved demo", func(p *Publication, i *Ingress, s *Service) {
			p.Spec.Hostname = "demo.local"
			i.Spec.Rules[0].Host = "demo.local"
		}},
		{"unsupported TLS", func(p *Publication, i *Ingress, s *Service) { p.Spec.TLS.Mode = "ACME" }},
		{"mdns without lan", func(p *Publication, i *Ingress, s *Service) { p.Spec.LAN = false }},
		{"loopback mdns", func(p *Publication, i *Ingress, s *Service) {
			p.Spec.Hostname = "app.example.localhost"
			i.Spec.Rules[0].Host = p.Spec.Hostname
		}},
		{"disabled", func(p *Publication, i *Ingress, s *Service) { p.Spec.Enabled = false }},
		{"wrong class", func(p *Publication, i *Ingress, s *Service) { i.Spec.Class = OutputClass }},
		{"annotation", func(p *Publication, i *Ingress, s *Service) {
			i.Metadata.Annotations = map[string]string{"traefik.ingress.kubernetes.io/router.middlewares": "evil"}
		}},
		{"ingress TLS", func(p *Publication, i *Ingress, s *Service) { i.Spec.TLS = []json.RawMessage{json.RawMessage(`{}`)} }},
		{"default backend", func(p *Publication, i *Ingress, s *Service) { i.Spec.DefaultBackend = json.RawMessage(`{}`) }},
		{"external name", func(p *Publication, i *Ingress, s *Service) { s.Spec.Type = "ExternalName" }},
		{"manual endpoints", func(p *Publication, i *Ingress, s *Service) { s.Spec.Selector = nil }},
		{"external IPs", func(p *Publication, i *Ingress, s *Service) { s.Spec.ExternalIPs = []string{"127.0.0.1"} }},
		{"headless", func(p *Publication, i *Ingress, s *Service) { s.Spec.ClusterIP = "None" }},
		{"missing service port", func(p *Publication, i *Ingress, s *Service) { s.Spec.Ports[0].Port = 999 }},
		{"resource backend", func(p *Publication, i *Ingress, s *Service) {
			i.Spec.Rules[0].HTTP.Paths[0].Backend.Resource = json.RawMessage(`{}`)
		}},
		{"missing HTTP paths", func(p *Publication, i *Ingress, s *Service) { i.Spec.Rules[0].HTTP.Paths = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, i, s := fixture(t)
			tc.mutate(&p, &i, &s)
			if _, _, err := BuildRoute(p, i, map[string]Service{"example/web": s}); err == nil {
				t.Fatal("accepted invalid publication")
			}
		})
	}
	p, i, s := fixture(t)
	r, out, err := BuildRoute(p, i, map[string]Service{"example/web": s})
	if err != nil {
		t.Fatal(err)
	}
	if out.Spec.Class != OutputClass || out.Spec.Rules[0].Host != p.Spec.Hostname || out.Metadata.Name != "ca-pub-"+p.Metadata.UID || out.Metadata.Annotations != nil {
		t.Fatal(out)
	}
	if err := (Snapshot{Items: []Record{r}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Snapshot{Items: []Record{r, r}}).Validate(); err == nil {
		t.Fatal("duplicate host accepted")
	}
}

type fakeAPI struct {
	pubs      []Publication
	ingress   Ingress
	service   Service
	generated map[string]Ingress
	status    map[string]Status
	fail      string
}

func (a *fakeAPI) run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if args[0] == a.fail {
		return nil, errors.New("injected API error")
	}
	switch args[0] {
	case "get":
		items := []any{a.ingress}
		raw, _ := json.Marshal(a.service)
		var svc map[string]any
		json.Unmarshal(raw, &svc)
		svc["kind"] = "Service"
		items = append(items, svc)
		for _, p := range a.pubs {
			items = append(items, p)
		}
		for _, i := range a.generated {
			items = append(items, i)
		}
		return json.Marshal(map[string]any{"items": items})
	case "apply":
		var list struct {
			Items []Ingress `json:"items"`
		}
		if err := json.Unmarshal(input, &list); err != nil {
			return nil, err
		}
		for _, i := range list.Items {
			a.generated[i.Metadata.Name] = i
		}
	case "delete":
		delete(a.generated, args[2])
	case "patch":
		var ops []struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal([]byte(args[len(args)-1]), &ops); err != nil {
			return nil, err
		}
		var status Status
		if err := json.Unmarshal(ops[2].Value, &status); err != nil {
			return nil, err
		}
		a.status[args[2]] = status
	default:
		return nil, errors.New("unexpected command")
	}
	return nil, nil
}

func TestReconcileUpdateConflictDeleteAndRestart(t *testing.T) {
	p, i, s := fixture(t)
	api := &fakeAPI{pubs: []Publication{p}, ingress: i, service: s, generated: map[string]Ingress{}, status: map[string]Status{}}
	c := &Controller{Run: api.run}
	reconcile := func() {
		t.Helper()
		if err := c.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	if len(c.snapshot.Items) != 1 || len(api.generated) != 1 {
		t.Fatal("route not programmed")
	}
	original := *c.snapshot
	api.fail = "apply"
	api.pubs[0].Spec.TLS.Mode = "HTTP"
	if c.Reconcile(context.Background()) == nil || !reflect.DeepEqual(*c.snapshot, original) {
		t.Fatal("apply failure destroyed last valid snapshot")
	}
	api.fail = ""
	reconcile()
	if c.snapshot.Items[0].TLS.Mode != "HTTP" {
		t.Fatal("TLS mode not changed")
	}
	api.pubs[0].Spec.Hostname = "new.example.local"
	api.ingress.Spec.Rules[0].Host = "new.example.local"
	api.pubs[0].Metadata.Generation++
	reconcile()
	if c.snapshot.Items[0].Hostname != "new.example.local" {
		t.Fatal("hostname not changed")
	}
	// A fresh controller reconstructs current state, not a saved host grant.
	c = &Controller{Run: api.run}
	reconcile()
	if c.snapshot.Items[0].Hostname != "new.example.local" {
		t.Fatal("restart did not converge")
	}
	conflict := api.pubs[0]
	conflict.Metadata.Name, conflict.Metadata.UID = "competitor", "publication-2"
	api.pubs = append(api.pubs, conflict)
	for range 2 {
		reconcile()
		if len(c.snapshot.Items) != 0 || len(api.generated) != 0 || api.status["web"].Conditions[0].Reason != "HostnameConflict" {
			t.Fatal("conflict not withdrawn")
		}
		slices.Reverse(api.pubs)
	}
	api.pubs = api.pubs[:1]
	reconcile()
	if len(c.snapshot.Items) != 1 {
		t.Fatal("conflict removal did not converge")
	}
	api.pubs[0].Spec.Enabled = false
	reconcile()
	if len(c.snapshot.Items) != 0 || len(api.generated) != 0 {
		t.Fatal("disable did not revoke")
	}
	api.pubs[0].Spec.Enabled = true
	reconcile()
	api.pubs = nil
	reconcile()
	if len(c.snapshot.Items) != 0 || len(api.generated) != 0 {
		t.Fatal("delete did not revoke")
	}
}

func TestFeedbackIdentityAndExpiry(t *testing.T) {
	p, i, s := fixture(t)
	api := &fakeAPI{pubs: []Publication{p}, ingress: i, service: s, generated: map[string]Ingress{}, status: map[string]Status{}}
	c := &Controller{Run: api.run}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	c.Routes(mux)
	send := func(f Feedback) {
		b, _ := json.Marshal([]Feedback{f})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/_agent/gateway/publication-status", bytes.NewReader(b)))
		if w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	f := Feedback{Record: c.snapshot.Items[0], Ready: true, CertificateReady: true, URL: "https://app.example.local/"}
	stale := f
	stale.Record.Generation++
	send(stale)
	if len(c.feedback) != 0 {
		t.Fatal("accepted stale generation")
	}
	stale = f
	stale.Record.UID = "replacement"
	send(stale)
	if len(c.feedback) != 0 {
		t.Fatal("accepted wrong UID")
	}
	send(f)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.status["web"].URL != f.URL || api.status["web"].Conditions[2].Status != "True" {
		t.Fatal(api.status)
	}
	o := c.feedback[p.Metadata.UID]
	o.at = time.Now().Add(-time.Minute * 2)
	c.feedback[p.Metadata.UID] = o
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.status["web"].Conditions[2].Status != "Unknown" {
		t.Fatal("stale host observation reported ready")
	}
	for _, body := range []string{`[{"unknown":true}]`, `[] {}`, strings.Repeat("x", 1<<20+1)} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/_agent/gateway/publication-status", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal("invalid feedback accepted")
		}
	}
}

func TestConditionsSeparateCertificateAvailabilityAndTrust(t *testing.T) {
	p, _, _ := fixture(t)
	now := time.Now()
	f := Feedback{CertificateReady: true, Ready: false, Error: "HTTP 503"}
	s := Conditions(p, "", "", &f, now)
	if s.Conditions[2].Status != "False" || s.Conditions[3].Status != "True" || !strings.Contains(s.Conditions[3].Message, "trust is not established") {
		t.Fatal(s)
	}
	p.Status = s
	next := Conditions(p, "", "", &f, now.Add(time.Minute))
	if !reflect.DeepEqual(next, s) {
		t.Fatal("unchanged conditions churn transition timestamps")
	}
}

func TestNamespaceMayPublishItsOwnBareName(t *testing.T) {
	for _, tc := range []struct {
		host, ns, name string
		ok             bool
	}{
		{"care.local", "care", "care", true},
		{"care.localhost", "care", "care-localhost", true},
		{"app.care.local", "care", "app", true},
		{"care.local", "other", "care", false},
		{"other.local", "care", "x", false},
		{"local", "local", "x", false},
		{"demo.local", "demo", "demo", false},
		{"demo.localhost", "demo", "demo", false},
		{"demo.local", "containers-demo", "demo", true},
		{"kite-system.local", "kite-system", "x", false},
		{"kube-x.local", "kube-x", "x", false},
		{"containers-gateway.local", "containers-gateway", "x", false},
	} {
		if got := AllowedHost(tc.host, tc.ns, tc.name); got != tc.ok {
			t.Errorf("%s in %s: got %t", tc.host, tc.ns, got)
		}
	}
}
