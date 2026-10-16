package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/mdlayher/vsock"
)

func guestMain() error {
	if len(os.Args) == 3 && os.Args[2] == "init-kite" {
		return initKiteSecrets("/var/lib")
	}
	var listener net.Listener
	var err error
	credentials, err := os.ReadFile("/sys/firmware/qemu_fw_cfg/by_name/opt/containers-anywhere/tls/raw")
	if err == nil {
		config, e := guestTLS(credentials, true)
		if e != nil {
			return e
		}
		listener, err = tls.Listen("tcp", ":8080", config)
	} else if os.IsNotExist(err) {
		listener, err = vsock.Listen(8080, nil)
	}
	if err != nil {
		return err
	}
	defer listener.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_agent/logs", func(w http.ResponseWriter, r *http.Request) {
		cmd := exec.CommandContext(r.Context(), "journalctl", "--no-pager", "-n", "200", "-u", "k3s", "-u", "kite-secrets", "-u", "kite-forward", "-u", "containers-agent")
		cmd.Stdout, cmd.Stderr = w, w
		_ = cmd.Run()
		// Journals alone miss terminal pod states and service endpoints. Fixed,
		// read-only queries keep failures diagnosable without a shell endpoint.
		for _, args := range [][]string{
			{"get", "nodes,pods,pvc,svc,endpointslices", "-A", "-o", "wide"},
			{"get", "events", "-A", "--sort-by=.lastTimestamp"},
		} {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			cmd := exec.CommandContext(ctx, "k3s", append([]string{"kubectl", "--kubeconfig=/etc/rancher/k3s/k3s.yaml"}, args...)...)
			cmd.Stdout, cmd.Stderr = w, w
			_ = cmd.Run()
			cancel()
		}
	})
	mux.HandleFunc("GET /_agent/status", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		k3s := exec.CommandContext(ctx, "k3s", "kubectl", "--kubeconfig=/etc/rancher/k3s/k3s.yaml", "get", "--raw=/readyz").Run() == nil
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8081/healthz")
		kite := err == nil && resp.StatusCode == 200
		if resp != nil {
			resp.Body.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"healthy": k3s && kite, "k3s": k3s, "kite": kite})
	})
	mux.HandleFunc("POST /_agent/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := exec.Command("systemd-run", "--unit=containers-shutdown", "--on-active=2s", "--timer-property=AccuracySec=100ms", "systemctl", "poweroff", "--no-block").Run(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		io.WriteString(w, "Shutting down\n")
	})
	mux.HandleFunc("/_agent/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	target, _ := url.Parse("http://127.0.0.1:8081")
	mux.Handle("/", httputil.NewSingleHostReverseProxy(target))
	return (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(listener)
}

// The kite-secrets oneshot is the sole writer, ordered before K3s. Keep the
// existing Python-generated key file format so restarts never rotate identity.
func initKiteSecrets(varLib string) error {
	keys := filepath.Join(varLib, "containers-anywhere/kite-secrets.json")
	data := map[string]string{}
	b, err := os.ReadFile(keys)
	if os.IsNotExist(err) {
		data["JWT_SECRET"], data["KITE_ENCRYPT_KEY"] = randomToken(), randomToken()
		if err = saveJSON(keys, data); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err = json.Unmarshal(b, &data); err != nil {
		return fmt.Errorf("invalid persisted Kite keys: %w", err)
	}
	for _, key := range []string{"JWT_SECRET", "KITE_ENCRYPT_KEY"} {
		decoded, err := hex.DecodeString(data[key])
		if err != nil || len(decoded) != 32 {
			return fmt.Errorf("invalid persisted Kite key %s; refusing to regenerate", key)
		}
	}
	return saveJSON(filepath.Join(varLib, "rancher/k3s/server/manifests/00-kite-secrets.json"), map[string]any{
		"apiVersion": "v1", "kind": "List", "items": []any{
			map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"name": "kite-system"}},
			map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": "kite-secrets", "namespace": "kite-system"}, "stringData": data},
		},
	})
}
