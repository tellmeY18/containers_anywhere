// Package publication defines the workload-to-host publication contract.
package publication

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	Group           = "networking.containersanywhere.dev"
	Resource        = "workloadpublications." + Group
	InputClass      = "containers-anywhere"
	OutputClass     = "containers-anywhere-published"
	MaxPublications = 128
)

type Metadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	DeletionTimestamp *string           `json:"deletionTimestamp,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
}

type Publication struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
	Status     Status   `json:"status,omitempty"`
}

type Spec struct {
	IngressRef string `json:"ingressRef"`
	Hostname   string `json:"hostname"`
	Enabled    bool   `json:"enabled"`
	LAN        bool   `json:"lan"`
	MDNS       bool   `json:"mdns"`
	TLS        TLS    `json:"tls"`
}

type TLS struct {
	Mode string `json:"mode"`
}

type Record struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
	Revision   string `json:"revision"`
	Hostname   string `json:"hostname"`
	LAN        bool   `json:"lan"`
	MDNS       bool   `json:"mdns"`
	TLS        TLS    `json:"tls"`
}

type Snapshot struct {
	Items []Record `json:"items"`
}

type Feedback struct {
	Record           Record `json:"record"`
	URL              string `json:"url"`
	Ready            bool   `json:"ready"`
	CertificateReady bool   `json:"certificateReady"`
	Advertised       bool   `json:"advertised"`
	Error            string `json:"error,omitempty"`
	DiscoveryError   string `json:"discoveryError,omitempty"`
}

type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

type Status struct {
	ObservedGeneration int64       `json:"observedGeneration"`
	URL                string      `json:"url,omitempty"`
	Conditions         []Condition `json:"conditions"`
}

var label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var uid = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func AllowedHost(host, namespace, name string) bool {
	if !label.MatchString(namespace) || strings.HasPrefix(namespace, "kube-") ||
		namespace == "kite-system" || namespace == "containers-gateway" || len(host) > 253 {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	if namespace == "containers-demo" {
		return (name == "demo" && host == "demo.local") || (name == "demo-localhost" && host == "demo.localhost")
	}
	// A namespace may also publish its own bare name (care.local for namespace care).
	// "demo" is excluded because demo.local belongs to the built-in demo.
	if namespace != "demo" && (host == namespace+".local" || host == namespace+".localhost") {
		return true
	}
	return strings.HasSuffix(host, "."+namespace+".local") || strings.HasSuffix(host, "."+namespace+".localhost")
}

func (r Record) Validate() error {
	if !uid.MatchString(r.UID) || !label.MatchString(r.Name) || r.Generation < 1 ||
		!digest.MatchString(r.Revision) ||
		!AllowedHost(r.Hostname, r.Namespace, r.Name) {
		return errors.New("invalid publication identity or hostname")
	}
	if r.TLS.Mode != "LocalCA" && r.TLS.Mode != "HTTP" {
		return errors.New("unsupported TLS mode")
	}
	if r.MDNS && (!r.LAN || !strings.HasSuffix(r.Hostname, ".local")) {
		return errors.New("mDNS requires LAN and a .local hostname")
	}
	return nil
}

func (s Snapshot) Validate() error {
	if s.Items == nil || len(s.Items) > MaxPublications {
		return errors.New("invalid publication snapshot size")
	}
	hosts, ids := map[string]bool{}, map[string]bool{}
	for _, r := range s.Items {
		if err := r.Validate(); err != nil {
			return err
		}
		if hosts[r.Hostname] || ids[r.UID] {
			return errors.New("duplicate publication identity or hostname")
		}
		hosts[r.Hostname], ids[r.UID] = true, true
	}
	return nil
}

type Ingress struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       struct {
		Class          string            `json:"ingressClassName"`
		DefaultBackend json.RawMessage   `json:"defaultBackend,omitempty"`
		TLS            []json.RawMessage `json:"tls,omitempty"`
		Rules          []Rule            `json:"rules"`
	} `json:"spec"`
}

type Rule struct {
	Host string `json:"host"`
	HTTP struct {
		Paths []Path `json:"paths"`
	} `json:"http"`
}

type Path struct {
	Path     string `json:"path"`
	PathType string `json:"pathType"`
	Backend  struct {
		Service *struct {
			Name string `json:"name"`
			Port struct {
				Number int    `json:"number,omitempty"`
				Name   string `json:"name,omitempty"`
			} `json:"port"`
		} `json:"service,omitempty"`
		Resource json.RawMessage `json:"resource,omitempty"`
	} `json:"backend"`
}

type Service struct {
	Metadata Metadata `json:"metadata"`
	Spec     struct {
		Type        string            `json:"type"`
		ClusterIP   string            `json:"clusterIP"`
		Selector    map[string]string `json:"selector"`
		ExternalIPs []string          `json:"externalIPs"`
		Ports       []struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"ports"`
	} `json:"spec"`
}

// BuildRoute copies only the selected host's standard HTTP paths. No user
// annotations or arbitrary proxy configuration reach the generated route.
func BuildRoute(p Publication, ingress Ingress, services map[string]Service) (Record, Ingress, error) {
	r := Record{Name: p.Metadata.Name, Namespace: p.Metadata.Namespace, UID: p.Metadata.UID,
		Generation: p.Metadata.Generation, Hostname: p.Spec.Hostname,
		LAN: p.Spec.LAN, MDNS: p.Spec.MDNS, TLS: p.Spec.TLS}
	var out Ingress
	if !p.Spec.Enabled {
		return r, out, errors.New("publication disabled")
	}
	if ingress.Metadata.Namespace != p.Metadata.Namespace || ingress.Metadata.Name != p.Spec.IngressRef || ingress.Metadata.DeletionTimestamp != nil || ingress.Spec.Class != InputClass {
		return r, out, errors.New("same-namespace containers-anywhere Ingress not found")
	}
	if len(ingress.Spec.DefaultBackend) > 0 || len(ingress.Spec.TLS) > 0 {
		return r, out, errors.New("default backends and Ingress TLS are unsupported")
	}
	for k := range ingress.Metadata.Annotations {
		if strings.HasPrefix(k, "traefik.") || k == "kubernetes.io/ingress.class" {
			return r, out, errors.New("routing annotations are unsupported")
		}
	}
	var selected []Rule
	for _, rule := range ingress.Spec.Rules {
		if rule.Host == r.Hostname {
			selected = append(selected, rule)
		}
	}
	if len(selected) != 1 || len(selected[0].HTTP.Paths) == 0 {
		return r, out, errors.New("Ingress must contain exactly one matching host rule with HTTP paths")
	}
	for _, path := range selected[0].HTTP.Paths {
		if !strings.HasPrefix(path.Path, "/") || (path.PathType != "Prefix" && path.PathType != "Exact") || path.Backend.Service == nil || len(path.Backend.Resource) > 0 {
			return r, out, errors.New("only explicit Service backends with Prefix/Exact paths are supported")
		}
		ref := path.Backend.Service
		s, ok := services[p.Metadata.Namespace+"/"+ref.Name]
		if !ok || s.Metadata.DeletionTimestamp != nil || s.Spec.Type != "ClusterIP" || s.Spec.ClusterIP == "" || s.Spec.ClusterIP == "None" || len(s.Spec.Selector) == 0 || len(s.Spec.ExternalIPs) != 0 {
			return r, out, fmt.Errorf("Service %s must be a non-headless ClusterIP with a pod selector and no external IPs", ref.Name)
		}
		found := false
		for _, port := range s.Spec.Ports {
			if (port.Protocol == "" || port.Protocol == "TCP") && ((ref.Port.Number > 0 && ref.Port.Name == "" && port.Port == ref.Port.Number) || (ref.Port.Number == 0 && ref.Port.Name != "" && port.Name == ref.Port.Name)) {
				found = true
			}
		}
		if !found {
			return r, out, fmt.Errorf("Service %s has no matching TCP port", ref.Name)
		}
	}
	out.APIVersion, out.Kind = "networking.k8s.io/v1", "Ingress"
	out.Metadata.Name, out.Metadata.Namespace = "ca-pub-"+p.Metadata.UID, p.Metadata.Namespace
	out.Metadata.Labels = map[string]string{Group + "/managed": "true"}
	out.Spec.Class, out.Spec.Rules = OutputClass, selected
	b, _ := json.Marshal(out.Spec)
	sum := sha256.Sum256(b)
	r.Revision = hex.EncodeToString(sum[:])
	return r, out, r.Validate()
}

func Conditions(p Publication, reason, message string, feedback *Feedback, now time.Time) Status {
	s := Status{ObservedGeneration: p.Metadata.Generation, Conditions: []Condition{}}
	add := func(kind, state, why, msg string) {
		c := Condition{Type: kind, Status: state, Reason: why, Message: msg, ObservedGeneration: p.Metadata.Generation, LastTransitionTime: now.UTC().Format(time.RFC3339)}
		for _, old := range p.Status.Conditions {
			if old.Type == kind && old.Status == state {
				c.LastTransitionTime = old.LastTransitionTime
			}
		}
		s.Conditions = append(s.Conditions, c)
	}
	if reason != "" {
		for _, kind := range []string{"Accepted", "Programmed", "Ready", "CertificateReady", "Advertised"} {
			add(kind, "False", reason, message)
		}
		return s
	}
	add("Accepted", "True", "Accepted", "Validated publication and same-namespace route")
	if feedback == nil {
		for _, kind := range []string{"Programmed", "Ready", "CertificateReady", "Advertised"} {
			add(kind, "Unknown", "AwaitingHost", "Waiting for current authenticated host observation")
		}
		return s
	}
	s.URL = feedback.URL
	add("Programmed", "True", "Programmed", "Host bridge has applied this publication")
	for _, c := range []struct {
		kind    string
		ready   bool
		message string
	}{
		{"Ready", feedback.Ready, feedback.Error},
		{"CertificateReady", feedback.CertificateReady, "Local CA only; remote-client trust is not established"},
		{"Advertised", feedback.Advertised, feedback.DiscoveryError},
	} {
		state, why := "False", "NotReady"
		if c.ready {
			state, why = "True", "Ready"
		}
		if c.kind == "CertificateReady" && p.Spec.TLS.Mode == "HTTP" {
			why = "NotRequested"
		}
		if c.kind == "Advertised" && !p.Spec.MDNS {
			why = "NotRequested"
		}
		add(c.kind, state, why, c.message)
	}
	return s
}

func Sort(records []Record) {
	slices.SortFunc(records, func(a, b Record) int { return strings.Compare(a.Hostname, b.Hostname) })
}
