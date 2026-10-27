package guest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
)

const testManifests = "rancher/k3s/server/manifests"

var testSecret = platformSecret{Namespace: "kite-system", Name: "kite-secrets", Keys: []string{"JWT_SECRET", "KITE_ENCRYPT_KEY"}}

func TestPlatformSecretsPersistAndRecreateManifest(t *testing.T) {
	root := t.TempDir()
	keys := filepath.Join(root, "containers-anywhere/secrets/kite-system.kite-secrets.json")
	manifest := filepath.Join(root, testManifests, "00-secret-kite-system-kite-secrets.json")
	req := platformRequest{Secrets: []platformSecret{testSecret}}
	if err := applyPlatform(root, req); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(keys)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]string
	if err := json.Unmarshal(original, &data); err != nil || data["JWT_SECRET"] == data["KITE_ENCRYPT_KEY"] {
		t.Fatal("invalid or identical generated keys", err)
	}
	for _, path := range []string{keys, manifest} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("keys must be private", path, err)
		}
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	if err := applyPlatform(root, req); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(keys)
	if err != nil || string(after) != string(original) {
		t.Fatal("rotated persisted keys", err)
	}
	b, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			Kind       string
			StringData map[string]string
		}
	}
	if err := json.Unmarshal(b, &list); err != nil || len(list.Items) != 2 {
		t.Fatal("invalid manifest", err)
	}
	if list.Items[0].Kind != "Namespace" || list.Items[1].Kind != "Secret" || list.Items[1].StringData["JWT_SECRET"] != data["JWT_SECRET"] || list.Items[1].StringData["KITE_ENCRYPT_KEY"] != data["KITE_ENCRYPT_KEY"] {
		t.Fatal("manifest did not preserve identity")
	}
}

func TestPlatformSecretsAddKeysWithoutRotating(t *testing.T) {
	root := t.TempDir()
	keys := filepath.Join(root, "containers-anywhere/secrets/kite-system.kite-secrets.json")
	if err := applyPlatform(root, platformRequest{Secrets: []platformSecret{testSecret}}); err != nil {
		t.Fatal(err)
	}
	var before map[string]string
	b, _ := os.ReadFile(keys)
	if err := json.Unmarshal(b, &before); err != nil {
		t.Fatal(err)
	}
	extended := testSecret
	extended.Keys = append([]string{"EXTRA_KEY"}, testSecret.Keys...)
	if err := applyPlatform(root, platformRequest{Secrets: []platformSecret{extended}}); err != nil {
		t.Fatal(err)
	}
	var after map[string]string
	b, _ = os.ReadFile(keys)
	if err := json.Unmarshal(b, &after); err != nil {
		t.Fatal(err)
	}
	if after["JWT_SECRET"] != before["JWT_SECRET"] || after["KITE_ENCRYPT_KEY"] != before["KITE_ENCRYPT_KEY"] || len(after["EXTRA_KEY"]) != 64 {
		t.Fatal("existing keys rotated or new key missing")
	}
}

func TestPlatformSecretsExistingFiles(t *testing.T) {
	for _, content := range []string{
		`{"JWT_SECRET": "` + strings.Repeat("ab", 32) + `", "KITE_ENCRYPT_KEY": "` + strings.Repeat("cd", 32) + `"}`,
		`{`, `{"JWT_SECRET": 123}`,
		`{"JWT_SECRET":"` + strings.Repeat("z", 64) + `","KITE_ENCRYPT_KEY":"` + strings.Repeat("a", 64) + `"}`,
	} {
		t.Run(content, func(t *testing.T) {
			root := t.TempDir()
			keys := filepath.Join(root, "containers-anywhere/secrets/kite-system.kite-secrets.json")
			if err := atomicfile.WritePrivate(keys, []byte(content)); err != nil {
				t.Fatal(err)
			}
			err := applyPlatform(root, platformRequest{Secrets: []platformSecret{testSecret}})
			valid := strings.Contains(content, strings.Repeat("ab", 32))
			if (err == nil) != valid {
				t.Fatalf("valid=%v, error=%v", valid, err)
			}
			after, err := os.ReadFile(keys)
			if err != nil || string(after) != content {
				t.Fatal("overwrote existing keys", err)
			}
			if !valid {
				if _, err := os.Stat(filepath.Join(root, testManifests, "00-secret-kite-system-kite-secrets.json")); !os.IsNotExist(err) {
					t.Fatal("published invalid keys")
				}
			}
		})
	}
}

func TestPlatformManifestsAreWrittenIdempotentlyAndNeverPruned(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, testManifests, "platform-demo.yaml")
	req := platformRequest{Manifests: []platformManifest{{Name: "demo", Content: "kind: Namespace\n"}}}
	if err := applyPlatform(root, req); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyPlatform(root, req); err != nil {
		t.Fatal(err)
	}
	second, _ := os.Stat(path)
	if !second.ModTime().Equal(first.ModTime()) {
		t.Fatal("unchanged manifest was rewritten")
	}
	req.Manifests[0].Content = "kind: Namespace\nmetadata: {name: x}\n"
	if err := applyPlatform(root, req); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != req.Manifests[0].Content {
		t.Fatal("changed manifest was not updated")
	}
	if err := applyPlatform(root, platformRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("omitted manifest was deleted; workload deletion is not an appliance operation")
	}
}

func TestPlatformRequestRejectsUnsafeInput(t *testing.T) {
	for name, req := range map[string]platformRequest{
		"traversal":     {Manifests: []platformManifest{{Name: "../x", Content: "a"}}},
		"uppercase":     {Manifests: []platformManifest{{Name: "Demo", Content: "a"}}},
		"empty":         {Manifests: []platformManifest{{Name: "demo"}}},
		"duplicate":     {Manifests: []platformManifest{{Name: "a", Content: "x"}, {Name: "a", Content: "x"}}},
		"oversize":      {Manifests: []platformManifest{{Name: "a", Content: strings.Repeat("x", maxPlatformFile+1)}}},
		"secret ns":     {Secrets: []platformSecret{{Namespace: "../x", Name: "a", Keys: []string{"K"}}}},
		"secret key":    {Secrets: []platformSecret{{Namespace: "a", Name: "b", Keys: []string{"lower"}}}},
		"secret nokeys": {Secrets: []platformSecret{{Namespace: "a", Name: "b"}}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := applyPlatform(root, req); err == nil {
				t.Fatal("accepted unsafe platform request")
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Fatal("wrote files for a rejected request")
			}
		})
	}
}
