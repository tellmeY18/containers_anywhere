package guest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoveryKubeRejectsNonControllerCommands(t *testing.T) {
	mux := http.NewServeMux()
	recoveryKubeRoutes(mux)
	for _, body := range []string{
		`{"args":["proxy"]}`,
		`{"args":["config","view"]}`,
		`{"args":[]}`,
		`{"args":["get","pods"],"unexpected":true}`,
		`{"args":["get","pods"]} {}`,
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/_agent/recovery/kube", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid controller request returned %d", w.Code)
		}
	}
}

func TestRecoveryCommandOutputLimitCannotBeBypassedByCopy(t *testing.T) {
	out := &recoveryOutput{limit: 4}
	if _, err := io.Copy(out, io.LimitReader(strings.NewReader("too much output"), 100)); err == nil {
		t.Fatal("command output was not bounded")
	}
	if len(out.String()) > 4 {
		t.Fatal("buffer exceeded its limit")
	}
}
