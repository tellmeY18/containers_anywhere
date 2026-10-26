package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestAppReplacementRetainsInstalledApplianceAndData(t *testing.T) {
	m := testBundleManifest(t)
	original := bundleArchive(t, m)
	next := m
	next.Version = "0.1.1"
	recommended := bundleArchive(t, next)
	qemu := tarGz(t, map[string]string{"runtime.json": "{}"})
	var recommendedRequests atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/original", rangedHandler(original, true))
	mux.HandleFunc("/recommended", func(w http.ResponseWriter, r *http.Request) {
		recommendedRequests.Add(1)
		rangedHandler(recommended, true).ServeHTTP(w, r)
	})
	mux.Handle("/qemu", rangedHandler(qemu, true))
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	oldURL, oldSHA, oldRuntimeURL, oldRuntimeSHA := applianceURL, applianceSHA256, runtimeURL, runtimeSHA256
	oldClient := newDownloadClient
	t.Cleanup(func() {
		applianceURL, applianceSHA256, runtimeURL, runtimeSHA256 = oldURL, oldSHA, oldRuntimeURL, oldRuntimeSHA
		newDownloadClient = oldClient
	})
	newDownloadClient = server.Client
	applianceURL, applianceSHA256 = server.URL+"/original", sha256Text(original)
	runtimeURL, runtimeSHA256 = server.URL+"/qemu", sha256Text(qemu)
	state := t.TempDir()
	dir, err := bootstrap(state, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(filepath.Join(state, "release.json"), m); err != nil {
		t.Fatal(err)
	}
	disk := []byte("non-regenerable workload data, keys and cluster identity")
	if err := os.WriteFile(filepath.Join(state, "data.img"), disk, 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.ReadFile(filepath.Join(state, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	applianceURL, applianceSHA256 = server.URL+"/recommended", sha256Text(recommended)
	for _, cacheMissing := range []bool{false, true} {
		if cacheMissing {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}
		selected, err := bootstrap(state, func(string) {})
		if err != nil || selected != dir {
			t.Fatalf("app replacement changed selected appliance: %s, %v", selected, err)
		}
		if recommendedRequests.Load() != 0 {
			t.Fatal("consulted the new recommendation for an existing cluster")
		}
		if got, err := os.ReadFile(filepath.Join(state, "data.img")); err != nil || !bytes.Equal(got, disk) {
			t.Fatal("app replacement changed disk", err)
		}
		if got, err := os.ReadFile(filepath.Join(state, "release.json")); err != nil || !bytes.Equal(got, identity) {
			t.Fatal("app replacement changed identity", err)
		}
	}
	if _, err := bootstrap(t.TempDir(), func(string) {}); err != nil || recommendedRequests.Load() != 1 {
		t.Fatal("new cluster did not use the recommendation", err)
	}
}

func TestExistingClusterNeverFallsBackToRecommendedAppliance(t *testing.T) {
	for _, scenario := range []string{"missing-cache", "corrupt-identity", "unidentified-disk", "unsupported-protocol", "wrong-receipt"} {
		t.Run(scenario, func(t *testing.T) {
			state := t.TempDir()
			m := testBundleManifest(t)
			if scenario == "unsupported-protocol" {
				m.Protocol++
			}
			if scenario != "unidentified-disk" {
				if err := saveJSON(filepath.Join(state, "release.json"), m); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt-identity" {
				if err := os.WriteFile(filepath.Join(state, "release.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "wrong-receipt" {
				if err := saveJSON(filepath.Join(state, "appliance.json"), applianceSelection{URL: "https://example.com", SHA256: sha256Text(nil), BundleID: "not-this-disk"}); err != nil {
					t.Fatal(err)
				}
			}
			disk := []byte("retain original data")
			if err := os.WriteFile(filepath.Join(state, "data.img"), disk, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := bootstrap(state, func(string) {}); err == nil {
				t.Fatal("accepted unavailable or unidentified original appliance")
			}
			if got, err := os.ReadFile(filepath.Join(state, "data.img")); err != nil || !bytes.Equal(got, disk) {
				t.Fatal("modified existing disk", err)
			}
		})
	}
}
