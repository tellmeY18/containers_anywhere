package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
)

func validateLayers(m manifest) error {
	if m.Format != 3 || m.Product != "containers-anywhere" || m.Protocol != 1 ||
		m.DataBytes < 1<<30 || m.DataBytes > 64<<30 || len(m.Files) != 4 ||
		m.Kernel != "kernel" || m.Initrd != "initrd" {
		return errors.New("invalid Containers Anywhere bundle")
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
