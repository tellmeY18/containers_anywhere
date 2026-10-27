package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"
)

// Platform workloads (Kite, the gateway, the demo) are part of this app, not of
// the appliance. The app pushes them to the guest agent on every start; the
// agent publishes them to K3s, which pulls their pinned images at runtime. A new
// app release therefore updates them without replacing the appliance.
//
//go:embed platform/*.yaml
var platformFiles embed.FS

type platformManifest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type platformSecret struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Keys      []string `json:"keys"`
}

type platformPayload struct {
	Manifests []platformManifest `json:"manifests"`
	Secrets   []platformSecret   `json:"secrets"`
}

func platformRequest() ([]byte, error) {
	entries, err := platformFiles.ReadDir("platform")
	if err != nil {
		return nil, err
	}
	var p platformPayload
	for _, e := range entries {
		b, err := platformFiles.ReadFile(path.Join("platform", e.Name()))
		if err != nil {
			return nil, err
		}
		p.Manifests = append(p.Manifests, platformManifest{Name: strings.TrimSuffix(e.Name(), ".yaml"), Content: string(b)})
	}
	p.Secrets = []platformSecret{{Namespace: "kite-system", Name: "kite-secrets", Keys: []string{"JWT_SECRET", "KITE_ENCRYPT_KEY"}}}
	return json.Marshal(p)
}

func pushPlatform(ctx context.Context, client *http.Client, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://guest/_agent/platform", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("guest rejected platform workloads (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// maintainPlatform retries until the guest agent accepts the platform, which is
// reachable before K3s is healthy. Image pulls then proceed inside the guest.
func maintainPlatform(ctx context.Context, client *http.Client) {
	body, err := platformRequest()
	if err != nil {
		log.Printf("Platform workloads unavailable: %v", err)
		return
	}
	for failures := 0; ; failures++ {
		err := pushPlatform(ctx, client, body)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if failures%15 == 14 {
			log.Printf("Waiting to apply platform workloads: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
