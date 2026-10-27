package guest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
)

// The appliance applies, but does not own, platform workloads. The host app
// supplies manifests and the agent publishes them into the K3s add-on directory
// on the data disk, where K3s reconciles them. Removing a manifest from a later
// request never deletes it: workload deletion is not an appliance operation.
const (
	maxPlatformFile  = 512 << 10
	maxPlatformTotal = 2 << 20
	maxPlatformItems = 16
)

var (
	platformName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?$`)
	secretKey    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,62}$`)
)

type platformManifest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// platformSecret asks for random 32-byte hex values that are generated once,
// persisted on the data disk and never rotated or regenerated.
type platformSecret struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Keys      []string `json:"keys"`
}

type platformRequest struct {
	Manifests []platformManifest `json:"manifests"`
	Secrets   []platformSecret   `json:"secrets"`
}

func (r platformRequest) validate() error {
	if len(r.Manifests) > maxPlatformItems || len(r.Secrets) > maxPlatformItems {
		return errors.New("too many platform items")
	}
	total, seen := 0, map[string]bool{}
	for _, m := range r.Manifests {
		if !platformName.MatchString(m.Name) || seen[m.Name] {
			return fmt.Errorf("invalid or duplicate platform manifest name %q", m.Name)
		}
		seen[m.Name] = true
		if len(m.Content) == 0 || len(m.Content) > maxPlatformFile {
			return fmt.Errorf("platform manifest %s has an invalid size", m.Name)
		}
		total += len(m.Content)
	}
	if total > maxPlatformTotal {
		return errors.New("platform manifests are too large")
	}
	seen = map[string]bool{}
	for _, s := range r.Secrets {
		id := s.Namespace + "/" + s.Name
		if !platformName.MatchString(s.Namespace) || !platformName.MatchString(s.Name) || seen[id] || len(s.Keys) == 0 || len(s.Keys) > maxPlatformItems {
			return fmt.Errorf("invalid or duplicate platform secret %q", id)
		}
		seen[id] = true
		for _, k := range s.Keys {
			if !secretKey.MatchString(k) {
				return fmt.Errorf("invalid platform secret key %q", k)
			}
		}
	}
	return nil
}

func applyPlatform(varLib string, r platformRequest) error {
	if err := r.validate(); err != nil {
		return err
	}
	manifests := filepath.Join(varLib, "rancher/k3s/server/manifests")
	// Secrets first so a workload that references one does not start without it.
	for _, s := range r.Secrets {
		if err := ensureSecret(varLib, manifests, s); err != nil {
			return err
		}
	}
	for _, m := range r.Manifests {
		path := filepath.Join(manifests, "platform-"+m.Name+".yaml")
		if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, []byte(m.Content)) {
			continue
		}
		if err := atomicfile.Write(path, []byte(m.Content), 0600); err != nil {
			return err
		}
	}
	return nil
}

func ensureSecret(varLib, manifests string, s platformSecret) error {
	keys := filepath.Join(varLib, "containers-anywhere/secrets", s.Namespace+"."+s.Name+".json")
	data := map[string]string{}
	b, err := os.ReadFile(keys)
	existed := err == nil
	if err == nil {
		if err = json.Unmarshal(b, &data); err != nil {
			return fmt.Errorf("invalid persisted secret %s/%s: %w", s.Namespace, s.Name, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Existing values are validated, never replaced: rotating a key would lock
	// the workload out of its own data.
	for key, value := range data {
		if decoded, err := hex.DecodeString(value); err != nil || len(decoded) != 32 {
			return fmt.Errorf("invalid persisted secret key %s in %s/%s; refusing to regenerate", key, s.Namespace, s.Name)
		}
	}
	changed := false
	for _, key := range s.Keys {
		if _, ok := data[key]; !ok {
			data[key], changed = randomToken(), true
		}
	}
	if changed || !existed {
		if err := saveJSON(keys, data); err != nil {
			return err
		}
	}
	wanted := map[string]string{}
	for _, key := range s.Keys {
		wanted[key] = data[key]
	}
	return saveJSON(filepath.Join(manifests, "00-secret-"+s.Namespace+"-"+s.Name+".json"), map[string]any{
		"apiVersion": "v1", "kind": "List", "items": []any{
			map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"name": s.Namespace}},
			map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": s.Name, "namespace": s.Namespace}, "stringData": wanted},
		},
	})
}
