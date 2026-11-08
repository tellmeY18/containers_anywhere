package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tellmeY18/containers_anywhere/internal/recovery"
	"github.com/tellmeY18/containers_anywhere/internal/recoveryapi"
)

func TestRecoveryBackendPersistsIdentityBeforeGuestSubmission(t *testing.T) {
	previous := recoveryStopping.Swap(false)
	t.Cleanup(func() { recoveryStopping.Store(previous) })
	state := t.TempDir()
	called := false
	client := &http.Client{Transport: recoveryTestTransport(func(r *http.Request) (*http.Response, error) {
		called = true
		active, err := readRecoveryActivity(state)
		if err != nil || active.UID != "request-1" {
			t.Fatalf("submission preceded durable host journal: %v", err)
		}
		var req recovery.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.UID != "request-1" || req.Scope != "full" || req.Action != "capture" {
			t.Fatalf("incorrect executor request: %+v", req)
		}
		w := httptest.NewRecorder()
		_ = json.NewEncoder(w).Encode(recovery.Operation{Request: req, Phase: "restart-needed"})
		return w.Result(), nil
	})}
	b := &recoveryBridge{state: state, client: client}
	st, err := b.Submit(context.Background(), recoveryapi.ClusterOperation{UID: "request-1", Action: "Capture", Scope: "full"})
	if err != nil || st.Phase != "RestartRequired" || !called {
		t.Fatalf("submit: %+v, %v", st, err)
	}
	if b.restarting {
		t.Fatal("submission restarted the appliance before controller journal publication")
	}
}

func TestRecoveryBackendRejectsAnotherOperationStatus(t *testing.T) {
	state := t.TempDir()
	if err := recordRecoveryActivity(state, "request-1"); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: recoveryTestTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		_ = json.NewEncoder(w).Encode(recovery.Operation{Request: recovery.Request{UID: "request-2"}, Phase: "completed"})
		return w.Result(), nil
	})}
	b := &recoveryBridge{state: state, client: client}
	if _, err := b.Get(context.Background(), "request-1"); err == nil {
		t.Fatal("accepted another operation's success")
	}
	if err := checkRecoveryIdle(state); err == nil {
		t.Fatal("lost active recovery journal")
	}
}

func TestImportedRestoreCannotSelectClusterStateOrAnotherServiceAccount(t *testing.T) {
	for _, account := range []string{recoveryapi.WorkloadServiceAccount, "administrator"} {
		t.Run(account, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: recoveryTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/_agent/recovery/points/point-1" {
					t.Fatalf("unexpected mutation: %s %s", r.Method, r.URL.Path)
				}
				w := httptest.NewRecorder()
				_ = json.NewEncoder(w).Encode(recovery.Manifest{ID: "point-1", Scope: "full"})
				return w.Result(), nil
			})}
			b := &recoveryBridge{state: t.TempDir(), client: client}
			st, err := b.restoreImported(context.Background(), recoveryapi.ImportedRestoreRequest{RequestUID: "request-1", Namespace: "app", NamespaceUID: "namespace-1", ServiceAccount: account, PointID: "point-1", PVCNames: map[string]string{"source": "new-target"}})
			if err != nil || st.Phase != "Failed" {
				t.Fatalf("unsafe restore accepted: %+v %v", st, err)
			}
			if account == "administrator" && calls != 0 {
				t.Fatal("invalid delegated identity reached guest")
			}
		})
	}
}
