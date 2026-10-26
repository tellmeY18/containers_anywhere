package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
)

// Set by packaging from the exact build artifacts, never a mutable latest URL.
// The launcher is the trust anchor; a server-provided checksum is insufficient.
var applianceURL, applianceSHA256, runtimeURL, runtimeSHA256 string

// Overridable so tests can trust their own certificate authority.
var newDownloadClient = downloadClient

func downloadClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 || r.URL.Scheme != "https" {
			return errors.New("unsafe download redirect")
		}
		return nil
	}}
}

func bootstrap(state string, progress func(string)) (string, error) {
	root := filepath.Join(state, "appliances")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	client := newDownloadClient()
	address, digest := applianceURL, applianceSHA256
	want, err := readManifest(filepath.Join(state, "release.json"))
	existing := err == nil
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read installed appliance identity: %w", err)
	}
	if _, diskErr := os.Stat(filepath.Join(state, "data.img")); diskErr == nil && !existing {
		return "", errors.New("cluster disk has no appliance identity; restore a backup, not a new appliance")
	} else if diskErr != nil && !os.IsNotExist(diskErr) {
		return "", diskErr
	}
	if existing {
		if err := validateLayers(want); err != nil || want.Arch != runtime.GOARCH {
			return "", errors.New("installed appliance is incompatible with this launcher or host; retain the original launcher and appliance")
		}
		if dir, ok := cachedBundle(root, want); ok {
			progress("Retaining the exact appliance selected by this cluster.")
			if runtime.GOOS != "darwin" {
				if err := ensureRuntime(client, runtimeCache(state), progress); err != nil {
					return "", err
				}
			}
			return dir, nil
		}
		var selected applianceSelection
		raw, err := os.ReadFile(filepath.Join(state, "appliance.json"))
		if err != nil {
			return "", fmt.Errorf("original appliance is not cached; use --bundle with the exact original appliance: %w", err)
		}
		if err := json.Unmarshal(raw, &selected); err != nil || selected.BundleID != bundleID(want) {
			return "", errors.New("saved appliance download does not match cluster identity; use the exact original --bundle")
		}
		address, digest = selected.URL, selected.SHA256
	}
	if err := validDownload(address, digest); err != nil {
		return "", fmt.Errorf("no valid pinned appliance: %w; use a packaged release or the exact original --bundle", err)
	}
	dest := filepath.Join(root, digest)
	if err := os.MkdirAll(dest, 0700); err != nil {
		return "", err
	}
	if err := installArchive(client, address, digest, filepath.Join(dest, "bundle"), progress); err != nil {
		return "", err
	}
	m, err := verifyBundle(filepath.Join(dest, "bundle"))
	if err != nil {
		return "", err
	}
	if existing && !compatibleData(want, m) {
		return "", errors.New("downloaded appliance does not match the existing cluster; refusing replacement")
	}
	if err := saveJSON(filepath.Join(state, "appliance.json"), applianceSelection{address, digest, bundleID(m)}); err != nil {
		return "", err
	}
	if runtime.GOOS != "darwin" {
		if err := ensureRuntime(client, runtimeCache(state), progress); err != nil {
			return "", err
		}
	}
	return filepath.Join(dest, "bundle"), nil
}

type applianceSelection struct {
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	BundleID string `json:"bundle_id"`
}

func runtimeCache(state string) string {
	return filepath.Join(state, "runtimes", runtimeSHA256)
}

// Packaged runtimes are independently content-addressed. Explicit local bundles
// retain the historical sibling qemu/ convention.
func ensureRuntime(client *http.Client, applianceDir string, progress func(string)) error {
	if err := validDownload(runtimeURL, runtimeSHA256); err != nil {
		return fmt.Errorf("launcher has no valid pinned QEMU runtime: %w; use a packaged release", err)
	}
	if err := os.MkdirAll(applianceDir, 0700); err != nil {
		return err
	}
	return installArchive(client, runtimeURL, runtimeSHA256, filepath.Join(applianceDir, "qemu"), progress)
}

// cachedBundle finds an already-unpacked, fully verified appliance with the
// wanted content identity. Partial or tampered directories never qualify.
func cachedBundle(root string, want manifest) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name(), "bundle")
		m, err := readManifest(filepath.Join(dir, "manifest.json"))
		if err != nil || bundleID(m) != bundleID(want) {
			continue
		}
		if _, err := verifyBundle(dir); err != nil {
			continue
		}
		return dir, true
	}
	return "", false
}

func validDownload(address, digest string) error {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("download requires HTTPS")
	}
	b, err := hex.DecodeString(digest)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != digest {
		return errors.New("download requires a pinned SHA-256")
	}
	return nil
}

func installArchive(client *http.Client, address, digest, dest string, progress func(string)) error {
	if err := validDownload(address, digest); err != nil {
		return err
	}
	marker := filepath.Join(dest, ".archive-sha256")
	if b, err := os.ReadFile(marker); err == nil && string(b) == digest {
		return nil
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return errors.New("incomplete or untrusted runtime directory; move it aside before retrying: " + dest)
	}
	archive := dest + ".tar.gz"
	if err := downloadArchive(client, address, digest, archive, progress); err != nil {
		return err
	}
	progress("Unpacking and verifying " + filepath.Base(dest) + "…")
	stage, err := os.MkdirTemp(filepath.Dir(dest), "unpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	err = extractArchive(f, stage)
	f.Close()
	if err != nil {
		return err
	}
	if filepath.Base(dest) == "bundle" {
		m, err := verifyBundle(stage)
		if err != nil {
			return err
		}
		if m.Format != 3 || m.Arch != runtime.GOARCH {
			return errors.New("downloaded appliance is incompatible with this host")
		}
	}
	if err = atomicWrite(filepath.Join(stage, ".archive-sha256"), []byte(digest), 0600); err != nil {
		return err
	}
	if err = os.Rename(stage, dest); err != nil {
		return err
	}
	return os.Remove(archive)
}

// A partial download survives interruption. Resume is accepted only with the
// correct Content-Range; the complete archive is always hashed before use.
func downloadArchive(client *http.Client, address, digest, dest string, progress func(string)) error {
	if fileHashMatches(dest, digest) {
		return nil
	}
	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("GET", address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		f.Close()
		if fileHashMatches(part, digest) {
			return os.Rename(part, dest)
		}
		os.Remove(part)
		return errors.New("partial download is stale; retry to download again")
	}
	if resp.StatusCode == http.StatusPartialContent && offset > 0 {
		var start, end, total int64
		if n, _ := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); n != 3 || start != offset || end < start || total != end+1 || resp.ContentLength != end-start+1 {
			return errors.New("invalid download resume response")
		}
	} else if resp.StatusCode == http.StatusOK {
		offset = 0
		if err = f.Truncate(0); err != nil {
			return err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	if resp.ContentLength <= 0 || offset+resp.ContentLength > 8<<30 {
		return errors.New("download size missing or exceeds 8 GiB")
	}
	space, err := diskspace.Of(filepath.Dir(dest))
	if err != nil {
		return err
	}
	if uint64(resp.ContentLength)+(1<<30) > space.Free {
		return errors.New("insufficient disk space for appliance download")
	}
	progress(fmt.Sprintf("Downloading %s (%d MiB)…", filepath.Base(dest), (offset+resp.ContentLength)>>20))
	reader := &downloadProgress{reader: resp.Body, completed: offset, total: offset + resp.ContentLength, report: progress}
	n, err := io.Copy(f, io.LimitReader(reader, resp.ContentLength+1))
	if err != nil {
		return err
	}
	if n != resp.ContentLength {
		return errors.New("download interrupted or size changed; retry to resume")
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if !fileHashMatches(part, digest) {
		os.Remove(part)
		return errors.New("download checksum mismatch")
	}
	return os.Rename(part, dest)
}

type downloadProgress struct {
	reader           io.Reader
	completed, total int64
	last             time.Time
	report           func(string)
}

func (p *downloadProgress) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	p.completed += int64(n)
	if time.Since(p.last) > time.Second {
		p.report(fmt.Sprintf("Downloading: %d / %d MiB (%d%%)", p.completed>>20, p.total>>20, p.completed*100/p.total))
		p.last = time.Now()
	}
	return n, err
}

func extractArchive(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !filepath.IsLocal(h.Name) || strings.ContainsAny(h.Name, "\\:") || h.Name == ".archive-sha256" || h.Size < 0 {
			return errors.New("unsafe archive path")
		}
		target := filepath.Join(dest, filepath.FromSlash(h.Name))
		if h.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return errors.New("archive links and special files are forbidden")
		}
		total += h.Size
		if total > 16<<30 {
			return errors.New("unpacked archive exceeds 16 GiB")
		}
		space, err := diskspace.Of(dest)
		if err != nil {
			return err
		}
		if uint64(h.Size)+(256<<20) > space.Free {
			return errors.New("insufficient disk space to unpack appliance")
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if h.Mode&0111 != 0 {
			mode = 0700
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
	}
	_, err = io.Copy(io.Discard, gz)
	return err
}

func sha256Text(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
