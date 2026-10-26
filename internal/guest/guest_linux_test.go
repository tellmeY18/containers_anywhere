package guest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
)

func TestKiteSecretsPersistAndRecreateManifest(t *testing.T) {
	root := t.TempDir()
	keys := filepath.Join(root, "containers-anywhere/kite-secrets.json")
	manifest := filepath.Join(root, "rancher/k3s/server/manifests/00-kite-secrets.json")
	if err := initKiteSecrets(root); err != nil {
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
	if err := initKiteSecrets(root); err != nil {
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

func TestKiteSecretsExistingFiles(t *testing.T) {
	for _, content := range []string{
		`{"JWT_SECRET": "` + strings.Repeat("ab", 32) + `", "KITE_ENCRYPT_KEY": "` + strings.Repeat("cd", 32) + `"}`,
		`{`, `null`, `{}`, `{"JWT_SECRET": 123}`,
		`{"JWT_SECRET":"` + strings.Repeat("z", 64) + `","KITE_ENCRYPT_KEY":"` + strings.Repeat("a", 64) + `"}`,
	} {
		t.Run(content, func(t *testing.T) {
			root := t.TempDir()
			keys := filepath.Join(root, "containers-anywhere/kite-secrets.json")
			if err := atomicfile.WritePrivate(keys, []byte(content)); err != nil {
				t.Fatal(err)
			}
			err := initKiteSecrets(root)
			valid := strings.Contains(content, strings.Repeat("ab", 32))
			if (err == nil) != valid {
				t.Fatalf("valid=%v, error=%v", valid, err)
			}
			after, err := os.ReadFile(keys)
			if err != nil || string(after) != content {
				t.Fatal("overwrote existing keys", err)
			}
			if !valid {
				if _, err := os.Stat(filepath.Join(root, "rancher/k3s/server/manifests/00-kite-secrets.json")); !os.IsNotExist(err) {
					t.Fatal("published invalid keys")
				}
			}
		})
	}
}

func TestKiteInitializationDoesNotProvisionApplications(t *testing.T) {
	root := t.TempDir()
	if err := initKiteSecrets(root); err != nil {
		t.Fatal(err)
	}
	applications := []string{
		"containers-anywhere/cncm-bootstrap.json",
		"rancher/k3s/server/manifests/00-cncm-bootstrap.json",
		"rancher/k3s/server/manifests/cncm.yaml",
	}
	for _, name := range applications {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("created application state %s: %v", name, err)
		}
	}
}
