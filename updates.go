package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tellmeY18/containers_anywhere/internal/protocol"
)

func validateLayers(m manifest) error {
	if m.Format != 3 || m.Product != "containers-anywhere" || !protocol.Supports(m.Protocol, m.Capabilities) ||
		m.DataBytes < 1<<30 || m.DataBytes > 64<<30 || len(m.Files) != 4 ||
		m.Kernel != "kernel" || m.Initrd != "initrd" {
		return errors.New("unsupported Containers Anywhere bundle: current protocol and capabilities required; use a current appliance with a new empty state directory")
	}
	return nil
}

// MVP deliberately pins existing data to its exact appliance. K3s upgrades and
// rollbacks need a tested migration contract, not a version-string comparison.
func compatibleData(old, next manifest) bool {
	return old.Format == 3 && next.Format == 3 && bundleID(old) == bundleID(next)
}

func readManifest(path string) (manifest, error) {
	var m manifest
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	return m, err
}

func bundleID(m manifest) string {
	b, _ := json.Marshal(m)
	return sha256Text(b)
}

func fileHashMatches(path, want string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	return err == nil && hex.EncodeToString(h.Sum(nil)) == want
}

func verifyRuntime(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if err != nil {
		return err
	}
	var files map[string]string
	if err = json.Unmarshal(b, &files); err != nil {
		return err
	}
	name := "qemu-system-x86_64"
	if runtime.GOARCH == "arm64" {
		name = "qemu-system-aarch64"
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if files[name] == "" {
		return fmt.Errorf("QEMU runtime missing checksum: %s", name)
	}
	for name, want := range files {
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
			return fmt.Errorf("invalid QEMU runtime filename")
		}
		if !fileHashMatches(filepath.Join(dir, filepath.FromSlash(name)), want) {
			return fmt.Errorf("QEMU checksum mismatch or non-regular member: %s", name)
		}
	}
	return nil
}
