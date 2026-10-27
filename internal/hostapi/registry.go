package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Registry trust is administrator-approved, one endpoint per resource, with no
// wildcards. It becomes a containerd hosts.toml beneath the data disk, which
// containerd reads without a restart and without any change to the root image.
const registryMarker = "# managed by containers-anywhere HostRegistryTrust; do not edit\n"

type TrustSpec struct {
	Endpoint    string `json:"endpoint"`
	PlainHTTP   bool   `json:"plainHTTP"`
	Description string `json:"description,omitempty"`
}

type TrustStatus struct {
	Phase              string `json:"phase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	UpdatedAt          string `json:"updatedAt,omitempty"`
}

type Trust struct {
	Metadata Meta        `json:"metadata"`
	Spec     TrustSpec   `json:"spec"`
	Status   TrustStatus `json:"status"`
}

// ValidateEndpoint permits host:port where the host is an IP literal that is
// loopback or belongs to this node, or the name localhost. Anything that could
// widen trust (names, wildcards, schemes, paths, credentials) is refused.
func ValidateEndpoint(endpoint string, local []net.IP) error {
	if endpoint == "" || len(endpoint) > 64 || strings.ContainsAny(endpoint, "/\\@*?#% \t\r\n") || strings.Contains(endpoint, "://") {
		return errors.New("endpoint must be host:port with no scheme, path, wildcard or credentials")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return errors.New("endpoint must be host:port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return errors.New("port must be 1-65535")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("host must be an IP address of this node, or localhost")
	}
	if ip.IsLoopback() {
		return nil
	}
	for _, l := range local {
		if l.Equal(ip) {
			return nil
		}
	}
	return errors.New("host must be a loopback or node address; remote registries are not trusted")
}

func HostsToml(endpoint string) string {
	return registryMarker + fmt.Sprintf("server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n", "http://"+endpoint, "http://"+endpoint)
}

type trustState struct {
	Endpoints []string `json:"endpoints"`
}

func (c *Controller) patchTrust(ctx context.Context, t Trust, st TrustStatus) error {
	st.UpdatedAt = c.now().UTC().Format(time.RFC3339)
	st.ObservedGeneration = t.Metadata.Generation
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": t.Metadata.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": t.Metadata.ResourceVersion},
		{"op": "add", "path": "/status", "value": st},
	})
	_, err := c.Run(ctx, nil, "patch", RegistryTrustResource, t.Metadata.Name, "--subresource=status", "--type=json", "-p", string(patch))
	return err
}

// ReconcileRegistryTrust makes certsDir match the approved resources. It only
// ever removes files it wrote itself (recorded in statePath and carrying the
// marker), so registry configuration it does not own is never touched.
func (c *Controller) ReconcileRegistryTrust(ctx context.Context, certsDir, statePath string, local []net.IP) error {
	b, err := c.Run(ctx, nil, "get", RegistryTrustResource, "-o", "json")
	if err != nil {
		return err
	}
	var list struct {
		Items []Trust `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	// Competing claims on one endpoint fail closed.
	claims := map[string]int{}
	for _, t := range list.Items {
		if t.Metadata.DeletionTimestamp == nil {
			claims[t.Spec.Endpoint]++
		}
	}
	desired := map[string]string{}
	verdict := map[string]TrustStatus{}
	for _, t := range list.Items {
		id := t.Metadata.UID
		switch {
		case t.Metadata.DeletionTimestamp != nil:
			verdict[id] = TrustStatus{Phase: "Revoked", Reason: "Deleting", Message: "Trust is being removed"}
		case !t.Spec.PlainHTTP:
			verdict[id] = TrustStatus{Phase: "Rejected", Reason: "Unsupported", Message: "Only plain-HTTP registries are supported; plainHTTP must be true"}
		case claims[t.Spec.Endpoint] > 1:
			verdict[id] = TrustStatus{Phase: "Rejected", Reason: "Duplicate", Message: "More than one resource claims this endpoint; all claims are withdrawn"}
		default:
			if err := ValidateEndpoint(t.Spec.Endpoint, local); err != nil {
				verdict[id] = TrustStatus{Phase: "Rejected", Reason: "InvalidEndpoint", Message: err.Error()}
			} else {
				desired[t.Spec.Endpoint] = HostsToml(t.Spec.Endpoint)
				verdict[id] = TrustStatus{Phase: "Trusted", Reason: "Applied", Message: "containerd may pull from this endpoint over plain HTTP"}
			}
		}
	}
	var previous trustState
	if raw, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(raw, &previous)
	}
	var errs []error
	for _, old := range previous.Endpoints {
		if _, keep := desired[old]; !keep {
			if err := removeManaged(certsDir, old); err != nil {
				errs = append(errs, err)
				desired[old] = "" // retain the record so removal is retried
			}
		}
	}
	for endpoint, content := range desired {
		if content == "" {
			continue
		}
		if err := writeManaged(certsDir, endpoint, content); err != nil {
			errs = append(errs, err)
		}
	}
	next := trustState{}
	for endpoint := range desired {
		next.Endpoints = append(next.Endpoints, endpoint)
	}
	sort.Strings(next.Endpoints)
	if raw, err := json.Marshal(next); err == nil {
		if err := os.MkdirAll(filepath.Dir(statePath), 0700); err == nil {
			errs = append(errs, os.WriteFile(statePath, raw, 0600))
		}
	}
	for _, t := range list.Items {
		v := verdict[t.Metadata.UID]
		if t.Metadata.DeletionTimestamp != nil || (t.Status.Phase == v.Phase && t.Status.ObservedGeneration == t.Metadata.Generation && t.Status.Reason == v.Reason) {
			continue
		}
		errs = append(errs, c.patchTrust(ctx, t, v))
	}
	return errors.Join(errs...)
}

func managedPath(certsDir, endpoint string) (string, error) {
	if err := ValidateEndpoint(endpoint, nil); err != nil && !strings.Contains(err.Error(), "loopback or node") {
		return "", err
	}
	dir := filepath.Join(certsDir, endpoint)
	if rel, err := filepath.Rel(certsDir, dir); err != nil || rel != endpoint {
		return "", errors.New("endpoint escapes the registry directory")
	}
	return dir, nil
}

func writeManaged(certsDir, endpoint, content string) error {
	dir, err := managedPath(certsDir, endpoint)
	if err != nil {
		return err
	}
	file := filepath.Join(dir, "hosts.toml")
	if existing, err := os.ReadFile(file); err == nil {
		if string(existing) == content {
			return nil
		}
		if !strings.HasPrefix(string(existing), registryMarker) {
			return fmt.Errorf("%s already has registry configuration this agent did not write; refusing to replace it", endpoint)
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func removeManaged(certsDir, endpoint string) error {
	dir, err := managedPath(certsDir, endpoint)
	if err != nil {
		return err
	}
	file := filepath.Join(dir, "hosts.toml")
	existing, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(existing), registryMarker) {
		return nil // not ours any more; leave it alone
	}
	if err := os.Remove(file); err != nil {
		return err
	}
	_ = os.Remove(dir) // only succeeds when empty
	return nil
}
