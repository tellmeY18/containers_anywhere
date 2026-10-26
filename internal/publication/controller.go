package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"reflect"
	"sync"
	"time"
)

type Runner func(context.Context, []byte, ...string) ([]byte, error)

func Kubectl(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "k3s", append([]string{"kubectl", "--request-timeout=10s"}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl: %w: %.1024s", err, stderr.String())
	}
	return b, nil
}

type observation struct {
	feedback Feedback
	at       time.Time
}

type Controller struct {
	Run      Runner
	mu       sync.RWMutex
	snapshot *Snapshot
	feedback map[string]observation
	err      error
}

func (c *Controller) Reconcile(ctx context.Context) error {
	err := c.reconcile(ctx)
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	return err
}

func (c *Controller) reconcile(ctx context.Context) error {
	b, err := c.Run(ctx, nil, "get", Resource+",ingresses,services", "-A", "-o", "json")
	if err != nil {
		return err
	}
	if len(b) > 8<<20 {
		return errors.New("publication discovery exceeds 8 MiB")
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err = json.Unmarshal(b, &list); err != nil {
		return err
	}
	if list.Items == nil {
		return errors.New("discovery response has no items array")
	}
	pubs := []Publication{}
	ingresses, services := map[string]Ingress{}, map[string]Service{}
	existing := map[string]Ingress{}
	for _, raw := range list.Items {
		var obj struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		switch obj.Kind {
		case "WorkloadPublication":
			var p Publication
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			pubs = append(pubs, p)
		case "Ingress":
			var i Ingress
			if err := json.Unmarshal(raw, &i); err != nil {
				return err
			}
			key := i.Metadata.Namespace + "/" + i.Metadata.Name
			ingresses[key] = i
			if i.Spec.Class == OutputClass && i.Metadata.Labels[Group+"/managed"] == "true" {
				existing[key] = i
			}
		case "Service":
			var s Service
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			services[s.Metadata.Namespace+"/"+s.Metadata.Name] = s
		}
	}
	if len(pubs) > MaxPublications {
		return fmt.Errorf("publication limit exceeded (%d)", MaxPublications)
	}
	// All competing claims fail closed, independent of list order or timestamps.
	claims := map[string]int{}
	for _, p := range pubs {
		if p.Spec.Enabled && p.Metadata.DeletionTimestamp == nil && AllowedHost(p.Spec.Hostname, p.Metadata.Namespace, p.Metadata.Name) {
			claims[p.Spec.Hostname]++
		}
	}
	snapshot := Snapshot{Items: []Record{}}
	reasons, messages := map[string]string{}, map[string]string{}
	routes := []Ingress{}
	for _, p := range pubs {
		id := p.Metadata.UID
		if p.Metadata.DeletionTimestamp != nil {
			reasons[id], messages[id] = "Deleting", "Publication is being deleted"
			continue
		}
		if !p.Spec.Enabled {
			reasons[id], messages[id] = "Disabled", "Publication is disabled"
			continue
		}
		if claims[p.Spec.Hostname] > 1 {
			reasons[id], messages[id] = "HostnameConflict", "All competing hostname claims are withdrawn"
			continue
		}
		r, out, err := BuildRoute(p, ingresses[p.Metadata.Namespace+"/"+p.Spec.IngressRef], services)
		if err != nil {
			reasons[id], messages[id] = "InvalidRoute", err.Error()
			continue
		}
		snapshot.Items = append(snapshot.Items, r)
		routes = append(routes, out)
		delete(existing, out.Metadata.Namespace+"/"+out.Metadata.Name)
	}
	Sort(snapshot.Items)
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if len(routes) > 0 {
		b, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": routes})
		if err != nil {
			return err
		}
		if _, err = c.Run(ctx, b, "apply", "--server-side", "--field-manager=containers-publication", "-f", "-"); err != nil {
			return err
		}
	}
	for _, i := range existing {
		// The admission policy reserves this class and prefix to the controller.
		if _, err = c.Run(ctx, nil, "delete", "ingress", i.Metadata.Name, "-n", i.Metadata.Namespace, "--ignore-not-found", "--wait=false"); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.snapshot = &snapshot
	if c.feedback == nil {
		c.feedback = map[string]observation{}
	}
	for id := range c.feedback {
		found := false
		for _, r := range snapshot.Items {
			if r.UID == id {
				found = true
				break
			}
		}
		if !found {
			delete(c.feedback, id)
		}
	}
	observations := map[string]observation{}
	for id, o := range c.feedback {
		observations[id] = o
	}
	c.mu.Unlock()
	now := time.Now()
	for _, p := range pubs {
		var f *Feedback
		for _, r := range snapshot.Items {
			if o, ok := observations[r.UID]; r.UID == p.Metadata.UID && ok && o.feedback.Record == r && now.Sub(o.at) < 60*time.Second {
				copy := o.feedback
				f = &copy
			}
		}
		status := Conditions(p, reasons[p.Metadata.UID], messages[p.Metadata.UID], f, now)
		if reflect.DeepEqual(status, p.Status) {
			continue
		}
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": p.Metadata.UID},
			{"op": "test", "path": "/metadata/resourceVersion", "value": p.Metadata.ResourceVersion},
			{"op": "add", "path": "/status", "value": status},
		})
		if _, err = c.Run(ctx, nil, "patch", Resource, p.Metadata.Name, "-n", p.Metadata.Namespace, "--subresource=status", "--type=json", "-p", string(patch)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /_agent/gateway/publications", func(w http.ResponseWriter, r *http.Request) {
		c.mu.RLock()
		defer c.mu.RUnlock()
		if c.snapshot == nil || c.err != nil {
			http.Error(w, "Publication reconciliation unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(c.snapshot)
	})
	mux.HandleFunc("POST /_agent/gateway/publication-status", func(w http.ResponseWriter, r *http.Request) {
		var feedback []Feedback
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&feedback); err != nil || len(feedback) > MaxPublications {
			http.Error(w, "Invalid feedback", 400)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "Invalid feedback", 400)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.snapshot == nil {
			http.Error(w, "No current snapshot", 409)
			return
		}
		if c.feedback == nil {
			c.feedback = map[string]observation{}
		}
		for _, f := range feedback {
			for _, record := range c.snapshot.Items {
				if f.Record == record {
					c.feedback[record.UID] = observation{f, time.Now()}
					break
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
