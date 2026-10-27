package guest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/tellmeY18/containers_anywhere/internal/boottls"
	"github.com/tellmeY18/containers_anywhere/internal/protocol"
)

func Main() error {
	var listener net.Listener
	var err error
	credentials, err := os.ReadFile("/sys/firmware/qemu_fw_cfg/by_name/opt/containers-anywhere/tls/raw")
	if err == nil {
		config, e := boottls.Config(credentials, true)
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
	gateway := &guestGateway{}
	go gateway.refresh(context.Background())
	gateway.routes(mux)
	mux.HandleFunc("GET /_agent/logs", func(w http.ResponseWriter, r *http.Request) {
		cmd := exec.CommandContext(r.Context(), "journalctl", "--no-pager", "-n", "200", "-u", "k3s", "-u", "console-forward", "-u", "containers-agent")
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
		console := err == nil && resp.StatusCode == 200
		if resp != nil {
			resp.Body.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"healthy": k3s, "k3s": k3s, "console": console, "protocol": protocol.Version, "capabilities": protocol.Capabilities()})
	})
	mux.HandleFunc("PUT /_agent/platform", func(w http.ResponseWriter, r *http.Request) {
		var req platformRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPlatformTotal+(256<<10))).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := req.validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := applyPlatform("/var/lib", req); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "Platform applied\n")
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
