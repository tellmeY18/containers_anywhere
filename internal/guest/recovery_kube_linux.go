package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// recoveryKubeRoutes is the private transport for the app-owned recovery
// controller. It is reachable only over the launcher's vsock/per-boot mTLS
// connection; the browser proxy must never forward /_agent paths. Kubernetes
// authorization for workload requests is enforced by the CRDs and controller.
// No host credential or command transport is given to a workload.
func recoveryKubeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /_agent/recovery/kube", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Args  []string `json:"args"`
			Input string   `json:"input"`
		}
		// JSON can expand each raw byte to a six-byte Unicode escape. The
		// decoded input+arguments remain bounded separately to 8 MiB.
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || len(in.Args) == 0 || len(in.Args) > 256 || len(in.Input) > 8<<20 {
			http.Error(w, "invalid recovery API request", http.StatusBadRequest)
			return
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			http.Error(w, "recovery API request must contain one object", http.StatusBadRequest)
			return
		}
		switch in.Args[0] {
		case "get", "create", "apply", "patch", "delete", "exec", "annotate":
		default:
			http.Error(w, "unsupported recovery API operation", http.StatusBadRequest)
			return
		}
		total := len(in.Input)
		for _, arg := range in.Args {
			total += len(arg)
			if len(arg) > 2<<20 || strings.ContainsRune(arg, '\x00') {
				http.Error(w, "invalid recovery API argument", http.StatusBadRequest)
				return
			}
		}
		if total > 8<<20 {
			http.Error(w, "recovery API input exceeds limit", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		args := append([]string{"kubectl", "--kubeconfig=/etc/rancher/k3s/k3s.yaml", "--request-timeout=120s"}, in.Args...)
		cmd := exec.CommandContext(ctx, "k3s", args...)
		cmd.Stdin = strings.NewReader(in.Input)
		stdout := &recoveryOutput{limit: 8 << 20}
		stderr := &recoveryOutput{limit: 16 << 10}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		err := cmd.Run()
		w.Header().Set("Content-Type", "application/json")
		out := struct {
			Output string `json:"output"`
			Error  string `json:"error,omitempty"`
		}{Output: stdout.String()}
		if err != nil {
			out.Error = "Kubernetes operation failed: " + err.Error()
			// Exec hook output can contain application secrets; report only the
			// exit status. API errors are needed for idempotent reconciliation.
			if in.Args[0] != "exec" {
				out.Error += ": " + stderr.String()
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

type recoveryOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *recoveryOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, errors.New("recovery API output exceeds limit")
	}
	return b.buffer.Write(p)
}

func (b *recoveryOutput) String() string { return b.buffer.String() }

var _ io.Writer = (*recoveryOutput)(nil)
