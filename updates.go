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
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/tellmeY18/containers_anywhere/internal/protocol"
)

// maxDataDiskBytes is the largest data disk the launcher will create, restore or accept.
const maxDataDiskBytes = 256 << 30

func validateLayers(m manifest) error {
	if m.Format != 3 || m.Product != "containers-anywhere" || !protocol.Supports(m.Protocol, m.Capabilities) ||
		m.DataBytes < 1<<30 || m.DataBytes > maxDataDiskBytes || len(m.Files) != 4 ||
		m.Kernel != "kernel" || m.Initrd != "initrd" {
		return errors.New("unsupported Containers Anywhere bundle: current protocol and capabilities required; use a current appliance with a new empty state directory")
	}
	return nil
}

// Ordinary startup keeps existing data on its exact selected appliance. Changing
// it goes only through an explicit update or swap, never through a version compare.
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

type k3sVersion struct{ major, minor, patch, build int }

var k3sVersionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)\+k3s(\d+)$`)

func parseK3sVersion(s string) (k3sVersion, bool) {
	m := k3sVersionPattern.FindStringSubmatch(s)
	if m == nil {
		return k3sVersion{}, false
	}
	var v [4]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return k3sVersion{}, false
		}
		v[i] = n
	}
	return k3sVersion{v[0], v[1], v[2], v[3]}, true
}

// k3sChange classifies a K3s change between two appliances. Only an identical
// version is a root-only change. Within a minor, patch/build releases may move
// forward but never back. A different minor needs its own tested upgrade path.
func k3sChange(old, next string) (identical, upgrade bool) {
	if old != "" && old == next {
		return true, false
	}
	a, ok1 := parseK3sVersion(old)
	b, ok2 := parseK3sVersion(next)
	if !ok1 || !ok2 || a.major != b.major || a.minor != b.minor {
		return false, false
	}
	if b.patch != a.patch {
		return false, b.patch > a.patch
	}
	return false, b.build > a.build
}
