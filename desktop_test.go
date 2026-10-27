package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDesktopOffersGenericWorkloadsWithoutApplicationOnboarding(t *testing.T) {
	for _, removed := range []string{"CNCM", "CARE", "cncmURL", "/#token="} {
		if strings.Contains(desktopHTML, removed) {
			t.Fatalf("application onboarding remains: %s", removed)
		}
	}
	for _, required := range []string{"Open Kite", "gateway?.endpoints", "endpoint.ready", "endpoint.error", "endpoint.advertised", "textContent", "client reachability"} {
		if !strings.Contains(desktopHTML, required) {
			t.Fatalf("missing workload UI: %s", required)
		}
	}
}

func TestControlCallDoesNotFollowRedirectsOrLeakToken(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private" {
			t.Error("missing authentication")
		}
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer s.Close()
	if _, err := controlCall(control{URL: s.URL, Token: "private"}, "GET", "/status", nil, time.Second); err == nil {
		t.Fatal("accepted redirect")
	}
	if leaked {
		t.Fatal("followed redirect to another origin")
	}
}

func TestDesktopShowsWorkloadRequestsWithoutTrustingTheirText(t *testing.T) {
	for _, required := range []string{`id="requests"`, "host/requests", "'host/'+verb", "['approve','reject']", "confirm("} {
		if !strings.Contains(desktopHTML, required) {
			t.Fatalf("missing request approval UI: %s", required)
		}
	}
	// Request names and reasons come from workloads. They must only ever be set as
	// text, never parsed as markup.
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(desktopHTML, forbidden) {
			t.Fatalf("request text could be injected through %s", forbidden)
		}
	}
}
