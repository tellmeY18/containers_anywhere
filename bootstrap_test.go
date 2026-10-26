package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestArchiveRejectsUnsafeMembers(t *testing.T) {
	for _, h := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg},
		{Name: "/absolute", Typeflag: tar.TypeReg},
		{Name: `C:\escape`, Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"},
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "other"},
		{Name: ".archive-sha256", Typeflag: tar.TypeReg},
	} {
		t.Run(h.Name, func(t *testing.T) {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			tw.Close()
			gz.Close()
			if err := extractArchive(&b, t.TempDir()); err == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
}

func testBundleManifest(t *testing.T) manifest {
	t.Helper()
	dir, m := testBundle(t)
	_ = dir
	return m
}

func TestDownloadResumesAndVerifiesBeforePublish(t *testing.T) {
	data := []byte("the complete pinned archive")
	for _, ignoreRange := range []bool{false, true} {
		t.Run(fmt.Sprint(ignoreRange), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=4-" {
					t.Error("did not request resume")
				}
				body := data
				if !ignoreRange {
					body = data[4:]
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 4-%d/%d", len(data)-1, len(data)))
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					w.WriteHeader(206)
				}
				w.Write(body)
			}))
			defer s.Close()
			dest := filepath.Join(t.TempDir(), "archive")
			if err := os.WriteFile(dest+".part", data[:4], 0600); err != nil {
				t.Fatal(err)
			}
			if err := downloadArchive(s.Client(), s.URL, sha256Text(data), dest, func(string) {}); err != nil {
				t.Fatal(err)
			}
			if !fileHashMatches(dest, sha256Text(data)) {
				t.Fatal("published wrong bytes")
			}
		})
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "tampered") }))
	defer s.Close()
	dest := filepath.Join(t.TempDir(), "archive")
	if err := downloadArchive(s.Client(), s.URL, sha256Text(data), dest, func(string) {}); err == nil {
		t.Fatal("accepted corrupted archive")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("published corrupt archive")
	}
	if validDownload("http://example.com/a", sha256Text(data)) == nil || validDownload("https://example.com/a", "bad") == nil {
		t.Fatal("accepted unpinned/insecure download")
	}
}

func TestKiteProxySeparatesHostCapability(t *testing.T) {
	c := control{AppURL: "http://127.0.0.1:8484", Token: "control-only", AppToken: "private"}
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("ca_session"); err == nil {
			t.Error("host capability leaked")
		}
		w.WriteHeader(204)
	})
	h := kiteHandler(c, upstream)
	for _, tc := range []struct {
		path, cookie, origin string
		status               int
	}{
		{"/", "", "", 401}, {"/_agent/stop", "private", "", 404},
		{"/", "private", "https://evil.example", 403}, {"/", "private", c.AppURL, 204},
		{"/_open?token=private", "", "", 303},
	} {
		r := httptest.NewRequest("GET", c.AppURL+tc.path, nil)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "ca_session", Value: tc.cookie})
		}
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: got %d want %d", tc.path, w.Code, tc.status)
		}
	}
}

func bundleArchive(t *testing.T, m manifest) []byte {
	t.Helper()
	dir := t.TempDir()
	for name := range m.Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"manifest.json", "kernel", "initrd", "system.img", "data.img"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(raw))}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func rangedHandler(body []byte, ranged bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ranged {
			w.Write(body)
			return
		}
		http.ServeContent(w, r, "a.tar.gz", time.Unix(0, 0), bytes.NewReader(body))
	})
}

// App replacement must use the disk's identity, not the app's new recommendation.
func TestBootstrapReusesUnchangedAppliance(t *testing.T) {
	m := testBundleManifest(t)
	body := bundleArchive(t, m)
	server := httptest.NewTLSServer(rangedHandler(body, true))
	defer server.Close()
	newDownloadClient = server.Client
	defer func() { newDownloadClient = downloadClient }()
	state := t.TempDir()
	if err := saveJSON(filepath.Join(state, "release.json"), m); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "appliances")
	cached := filepath.Join(root, "oldarchivesha", "bundle")
	if err := extractArchive(bytes.NewReader(body), cached); err != nil {
		t.Fatal(err)
	}
	applianceURL, applianceSHA256, runtimeURL, runtimeSHA256 = server.URL, sha256Text(body), "", ""
	defer func() { applianceURL, applianceSHA256, runtimeURL, runtimeSHA256 = "", "", "", "" }()
	if runtime.GOOS != "darwin" {
		t.Skip("reuse path also installs the QEMU runtime off macOS")
	}
	dir, err := bootstrap(state, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if dir != cached {
		t.Fatalf("re-downloaded instead of reusing: %s", dir)
	}
}

func TestCachedBundleIgnoresTamperedContent(t *testing.T) {
	m := testBundleManifest(t)
	root := t.TempDir()
	dir := filepath.Join(root, "sha", "bundle")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	for name := range m.Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := cachedBundle(root, m); !ok {
		t.Fatal("rejected a valid cached bundle")
	}
	if err := os.WriteFile(filepath.Join(dir, "system.img"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := cachedBundle(root, m); ok {
		t.Fatal("reused a tampered bundle")
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func TestBootstrapInstallsIndependentRuntime(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS uses Virtualization.framework, not QEMU")
	}
	body := bundleArchive(t, testBundleManifest(t))
	qemu := tarGz(t, map[string]string{"runtime.json": "{}"})
	mux := http.NewServeMux()
	mux.Handle("/appliance", rangedHandler(body, true))
	mux.Handle("/qemu", rangedHandler(qemu, true))
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	newDownloadClient = server.Client
	defer func() { newDownloadClient = downloadClient }()
	applianceURL, applianceSHA256 = server.URL+"/appliance", sha256Text(body)
	runtimeURL, runtimeSHA256 = server.URL+"/qemu", sha256Text(qemu)
	defer func() { applianceURL, applianceSHA256, runtimeURL, runtimeSHA256 = "", "", "", "" }()
	state := t.TempDir()
	for _, pass := range []string{"fresh install", "cached reuse"} {
		_, err := bootstrap(state, func(string) {})
		if err != nil {
			t.Fatalf("%s: %v", pass, err)
		}
		if _, err := os.Stat(filepath.Join(runtimeCache(state), "qemu", "runtime.json")); err != nil {
			t.Fatalf("%s: runtime not in independent cache: %v", pass, err)
		}
	}
}
