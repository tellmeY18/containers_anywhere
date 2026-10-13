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
	"testing"
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
