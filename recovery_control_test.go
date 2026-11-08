package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoveryControlDoesNotExposeControllerTransport(t *testing.T) {
	for _, path := range []string{"/kube", "/points/../kube", "/points/a%2fb/export", "/points/a\\b/export", "/operations/id/delete"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			if recoveryControlAllowed(method, path) {
				t.Fatalf("exposed private controller path %s %s", method, path)
			}
		}
	}
	if recoveryControlAllowed("POST", "/points/example/export") {
		t.Fatal("an export must not allow mutations")
	}
}

type recoveryTestTransport func(*http.Request) (*http.Response, error)

func (f recoveryTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecoveryJSONRejectsGuestFailureAndOversize(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"guest refusal", 409, `{"error":"incompatible data"}`},
		{"oversized inventory", 200, strings.Repeat(" ", (8<<20)+1)},
		{"truncated result", 200, `{"phase":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: recoveryTestTransport(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				w.WriteHeader(tc.code)
				_, _ = w.WriteString(tc.body)
				return w.Result(), nil
			})}
			var result map[string]any
			if err := recoveryJSON(context.Background(), client, "GET", "/points", nil, &result); err == nil {
				t.Fatal("invalid guest result accepted")
			}
		})
	}
}
