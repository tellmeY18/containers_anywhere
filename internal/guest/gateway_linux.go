package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/publication"
)

// Discovery is control-plane only. Clinical request bytes never go through
// kubectl port-forward or the Kubernetes API server.
type guestGateway struct {
	mu           sync.RWMutex
	address      string
	publications publication.Controller
}

func (g *guestGateway) refresh(ctx context.Context) {
	g.publications.Run = publication.Kubectl
	for {
		func() {
			ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			b, err := exec.CommandContext(ctx, "k3s", "kubectl", "get", "service", "traefik", "-n", "containers-gateway", "-o", "json").Output()
			if err == nil {
				g.mu.Lock()
				g.address = ""
				g.mu.Unlock()
				var service struct {
					Spec struct {
						ClusterIP string `json:"clusterIP"`
						Type      string `json:"type"`
						Ports     []struct {
							Name string `json:"name"`
							Port int    `json:"port"`
						} `json:"ports"`
					} `json:"spec"`
				}
				if json.Unmarshal(b, &service) == nil && service.Spec.Type == "ClusterIP" && net.ParseIP(service.Spec.ClusterIP) != nil {
					for _, port := range service.Spec.Ports {
						if port.Name == "web" && port.Port > 0 && port.Port < 65536 {
							g.mu.Lock()
							g.address = net.JoinHostPort(service.Spec.ClusterIP, fmt.Sprint(port.Port))
							g.mu.Unlock()
						}
					}
				}
			}
		}()
		reconcileCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		if err := g.publications.Reconcile(reconcileCtx); err != nil {
			log.Printf("publication reconciliation: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (g *guestGateway) routes(mux *http.ServeMux) {
	connections := make(chan struct{}, 128)
	g.publications.Routes(mux)
	mux.HandleFunc("CONNECT /_agent/gateway/connect", func(w http.ResponseWriter, r *http.Request) {
		select {
		case connections <- struct{}{}:
			defer func() { <-connections }()
		default:
			http.Error(w, "Gateway connection limit", 503)
			return
		}
		g.mu.RLock()
		address := g.address
		g.mu.RUnlock()
		if address == "" {
			http.Error(w, "Gateway unavailable", 503)
			return
		}
		upstream, err := (&net.Dialer{Timeout: 5 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("169.254.254.1")}}).DialContext(r.Context(), "tcp", address)
		if err != nil {
			http.Error(w, "Gateway unavailable", 503)
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err = buffered.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(upstream, buffered); upstream.(*net.TCPConn).CloseWrite(); close(done) }()
		io.Copy(client, upstream)
		client.Close()
		upstream.Close()
		<-done
	})
}
