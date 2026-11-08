package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/recovery"
)

// serveRecoveryControl runs only after the control listener's host/token check.
// The browser and workload gateways never forward this namespace. Streaming
// exports/imports use the same authenticated VM transport without the short
// control client's timeout.
func serveRecoveryControl(w http.ResponseWriter, r *http.Request, client *http.Client, state string) {
	path := strings.TrimPrefix(r.URL.Path, "/control/recovery")
	if !recoveryControlAllowed(r.Method, path) || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if path == "/operations" && r.Method == http.MethodPost {
		var req recovery.Request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid recovery operation", http.StatusBadRequest)
			return
		}
		op, err := submitRecoveryOperation(r.Context(), state, client, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(op)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	deadline, _ := ctx.Deadline()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	// Retain the deadline through net/http's final buffered response flush.
	// The server owns resetting connection deadlines between requests.
	req, err := http.NewRequestWithContext(ctx, r.Method, "http://guest/_agent/recovery"+path, r.Body)
	if err != nil {
		http.Error(w, "invalid recovery request", http.StatusBadRequest)
		return
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	long := recoveryStreamClient(client)
	defer long.CloseIdleConnections()
	resp, err := long.Do(req)
	if err != nil {
		http.Error(w, "guest recovery executor unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	// An interrupted export must not look like a successfully terminated stream.
	// Its archive is also required to carry a verified terminal manifest.
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func recoveryStreamClient(client *http.Client) *http.Client {
	transport := client.Transport
	if t, ok := transport.(*http.Transport); ok {
		copy := t.Clone()
		copy.ResponseHeaderTimeout = 0
		transport = copy
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func recoveryControlAllowed(method, path string) bool {
	if strings.ContainsAny(path, "%\\") || strings.Contains(path, "..") {
		return false
	}
	p := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(p) == 1 {
		switch p[0] {
		case "points", "restart-needed", "capacity", "staging":
			return method == http.MethodGet
		case "operations":
			return method == http.MethodGet || method == http.MethodPost
		case "import":
			return method == http.MethodPost
		}
	}
	if len(p) == 2 && validRecoveryPathID(p[1]) {
		return (p[0] == "operations" && method == http.MethodGet) ||
			(p[0] == "workload-restore" && method == http.MethodGet) ||
			(p[0] == "staging" && method == http.MethodDelete) ||
			(p[0] == "points" && (method == http.MethodGet || method == http.MethodDelete))
	}
	return len(p) == 3 && p[0] == "points" && validRecoveryPathID(p[1]) && p[2] == "export" && method == http.MethodGet
}

func validRecoveryPathID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func recoveryKubectl(client *http.Client) func(context.Context, []byte, ...string) ([]byte, error) {
	return func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		var result struct {
			Output string `json:"output"`
			Error  string `json:"error"`
		}
		err := recoveryJSON(ctx, client, http.MethodPost, "/kube", struct {
			Args  []string `json:"args"`
			Input string   `json:"input"`
		}{args, string(input)}, &result)
		if err != nil {
			return nil, err
		}
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		return []byte(result.Output), nil
	}
}

// recoveryJSON is shared by the host controller and its executor adapter.
type recoveryHTTPError struct {
	Status  int
	Message string
}

func (e *recoveryHTTPError) Error() string {
	return fmt.Sprintf("recovery executor (%d): %s", e.Status, e.Message)
}

func recoveryJSON(ctx context.Context, client *http.Client, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://guest/_agent/recovery"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &recoveryHTTPError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<20))
		return err
	}
	limit := int64(8 << 20)
	if path == "/kube" {
		limit = 64 << 20 // JSON escaping can expand an 8 MiB raw result sixfold.
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return errors.New("recovery response exceeds limit")
	}
	return json.Unmarshal(b, out)
}
