package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestPlatformPayloadMatchesAgentLimits(t *testing.T) {
	b, err := platformRequest()
	if err != nil {
		t.Fatal(err)
	}
	var p platformPayload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?$`)
	names, total := map[string]bool{}, 0
	for _, m := range p.Manifests {
		if !name.MatchString(m.Name) || len(m.Content) == 0 || len(m.Content) > 512<<10 {
			t.Fatalf("manifest %q violates the guest agent contract", m.Name)
		}
		names[m.Name] = true
		total += len(m.Content)
	}
	if total > 2<<20 || len(p.Manifests) > 16 {
		t.Fatal("platform payload exceeds the guest agent limits")
	}
	for _, want := range []string{"kite", "gateway", "demo"} {
		if !names[want] {
			t.Fatalf("missing platform manifest %s", want)
		}
	}
}

func TestPlatformImagesArePulledAtRuntimeAndPinned(t *testing.T) {
	entries, _ := platformFiles.ReadDir("platform")
	image := regexp.MustCompile(`(?m)^\s*image:\s*(\S+)`)
	for _, e := range entries {
		b, _ := platformFiles.ReadFile("platform/" + e.Name())
		if strings.Contains(string(b), "imagePullPolicy: Never") {
			t.Fatalf("%s requires a preloaded image; platform images are pulled at runtime", e.Name())
		}
		for _, m := range image.FindAllStringSubmatch(string(b), -1) {
			if !strings.Contains(m[1], "@sha256:") {
				t.Fatalf("%s image %s is not pinned by digest", e.Name(), m[1])
			}
		}
	}
}

type platformTransport func(*http.Request) (*http.Response, error)

func (f platformTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMaintainPlatformRetriesUntilGuestAccepts(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: platformTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPut || r.URL.Path != "/_agent/platform" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		status := http.StatusServiceUnavailable
		if calls > 1 {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})}
	maintainPlatform(context.Background(), client)
	if calls != 2 {
		t.Fatalf("calls = %d, want a retry then success", calls)
	}
}
