package main

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
)

func guestMain() error {
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
