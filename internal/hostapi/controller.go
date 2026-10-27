package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Runner runs kubectl. The guest agent passes the real one; tests pass a fake.
type Runner func(ctx context.Context, input []byte, args ...string) ([]byte, error)

type Controller struct {
	Run Runner
	Now func() time.Time

	mu         sync.RWMutex
	facts      *Facts
	factsAt    time.Time
	requests   map[string]Request // actionable (Pending or Approved), keyed by UID
	status     *Status
	lastKey    string
	lastWrite  time.Time
	written    bool
	lastError  string
	crdMissing bool
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) SetFacts(f Facts) error {
	if err := f.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.facts, c.factsAt = &f, c.now()
	return nil
}

func objectApply(kind, name string) []byte {
	b, _ := json.Marshal(map[string]any{"apiVersion": Group + "/" + Version, "kind": kind, "metadata": map[string]string{"name": name}, "spec": map[string]any{}})
	return b
}

// ReconcileStatus publishes the merged facts. It writes at most every 30 seconds
// unless something material changed, so a quiet cluster is not churned.
func (c *Controller) ReconcileStatus(ctx context.Context, guest GuestFacts) error {
	c.mu.RLock()
	facts, at := c.facts, c.factsAt
	c.mu.RUnlock()
	now := c.now()
	st := Build(now, facts, at, guest)
	c.mu.Lock()
	c.status = &st
	key := st.MaterialKey()
	skip := c.written && key == c.lastKey && now.Sub(c.lastWrite) < 30*time.Second
	c.mu.Unlock()
	if skip {
		return nil
	}
	err := c.writeStatus(ctx, st)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.written, c.lastError = false, err.Error()
		return err
	}
	c.written, c.lastError, c.lastKey, c.lastWrite = true, "", key, now
	return nil
}

func (c *Controller) writeStatus(ctx context.Context, st Status) error {
	if _, err := c.Run(ctx, objectApply("HostStatus", SingletonName), "apply", "--server-side", "--field-manager=containers-host-api", "-f", "-"); err != nil {
		return err
	}
	patch, _ := json.Marshal([]map[string]any{{"op": "add", "path": "/status", "value": st}})
	_, err := c.Run(ctx, nil, "patch", StatusResource, SingletonName, "--subresource=status", "--type=json", "-p", string(patch))
	return err
}

func (c *Controller) patchRequest(ctx context.Context, r Request, st RequestStatus) error {
	st.UpdatedAt = c.now().UTC().Format(time.RFC3339)
	st.ObservedGeneration = r.Metadata.Generation
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": r.Metadata.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": r.Metadata.ResourceVersion},
		{"op": "add", "path": "/status", "value": st},
	})
	_, err := c.Run(ctx, nil, "patch", RequestResource, r.Metadata.Name, "-n", r.Metadata.Namespace, "--subresource=status", "--type=json", "-p", string(patch))
	return err
}

func (c *Controller) listRequests(ctx context.Context) ([]Request, error) {
	b, err := c.Run(ctx, nil, "get", RequestResource, "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	if len(b) > 4<<20 {
		return nil, errors.New("request list exceeds 4 MiB")
	}
	var list struct {
		Items []Request `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ReconcileRequests validates new requests, marks them Pending for the launcher
// and expires stale ones. Spec is immutable (admission policy), so what the human
// approves is exactly what is applied.
func (c *Controller) ReconcileRequests(ctx context.Context) error {
	items, err := c.listRequests(ctx)
	if err != nil {
		c.mu.Lock()
		c.requests = map[string]Request{}
		c.mu.Unlock()
		return err
	}
	now := c.now()
	actionable := map[string]Request{}
	var firstErr error
	for _, r := range items {
		if r.Metadata.DeletionTimestamp != nil || r.Metadata.UID == "" {
			continue
		}
		phase := r.Status.Phase
		if phase != "" && r.Status.ObservedGeneration != r.Metadata.Generation {
			phase = "" // the spec changed under an old decision; treat as new
		}
		var patchErr error
		switch {
		case terminal(phase):
		case phase == "":
			if verr := r.Spec.Validate(); verr != nil {
				patchErr = c.patchRequest(ctx, r, RequestStatus{Phase: PhaseRejected, Reason: "InvalidRequest", Message: verr.Error()})
			} else if patchErr = c.patchRequest(ctx, r, RequestStatus{Phase: PhasePending, Reason: "AwaitingApproval", Message: "Waiting for approval in the launcher"}); patchErr == nil {
				r.Status.Phase = PhasePending
				actionable[r.Metadata.UID] = r
			}
		case r.expired(now) && phase == PhasePending:
			patchErr = c.patchRequest(ctx, r, RequestStatus{Phase: PhaseExpired, Reason: "Expired", Message: "Not approved within 24 hours"})
		default:
			r.Status.Phase = phase
			actionable[r.Metadata.UID] = r
		}
		if patchErr != nil && firstErr == nil {
			firstErr = patchErr
		}
	}
	c.mu.Lock()
	c.requests = actionable
	c.mu.Unlock()
	return firstErr
}

// SetDecisions applies the launcher's decisions. Each request is re-read first,
// so a decision can only ever land on the exact object (UID) and the state it
// observed, never on a deleted-and-recreated namesake.
func (c *Controller) SetDecisions(ctx context.Context, decisions []Decision) error {
	var errs []error
	for _, d := range decisions {
		if err := d.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		c.mu.RLock()
		cached, ok := c.requests[d.UID]
		c.mu.RUnlock()
		if !ok {
			errs = append(errs, fmt.Errorf("request %s is not awaiting a decision", d.UID))
			continue
		}
		if !CanTransition(cached.Status.Phase, d.Phase) {
			errs = append(errs, fmt.Errorf("request %s cannot move from %s to %s", d.UID, cached.Status.Phase, d.Phase))
			continue
		}
		b, err := c.Run(ctx, nil, "get", RequestResource, cached.Metadata.Name, "-n", cached.Metadata.Namespace, "-o", "json")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var fresh Request
		if err := json.Unmarshal(b, &fresh); err != nil || fresh.Metadata.UID != d.UID || fresh.Status.Phase != cached.Status.Phase {
			errs = append(errs, fmt.Errorf("request %s changed; decision discarded", d.UID))
			continue
		}
		if err := c.patchRequest(ctx, fresh, RequestStatus{Phase: d.Phase, Reason: d.Reason, Message: d.Message, Result: d.Result}); err != nil {
			errs = append(errs, err)
			continue
		}
		c.mu.Lock()
		if terminal(d.Phase) {
			delete(c.requests, d.UID)
		} else {
			cached.Status.Phase = d.Phase
			c.requests[d.UID] = cached
		}
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (c *Controller) Actionable() []Actionable {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := []Actionable{}
	for _, r := range c.requests {
		out = append(out, Actionable{UID: r.Metadata.UID, Namespace: r.Metadata.Namespace, Name: r.Metadata.Name,
			Generation: r.Metadata.Generation, Created: r.Metadata.CreationTimestamp, Phase: r.Status.Phase, Spec: r.Spec})
	}
	return out
}

type Snapshot struct {
	Status      *Status `json:"status"`
	Written     bool    `json:"written"`
	LastError   string  `json:"lastError,omitempty"`
	CRDsMissing bool    `json:"crdsMissing,omitempty"`
}

func (c *Controller) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Snapshot{Status: c.status, Written: c.written, LastError: c.lastError, CRDsMissing: c.crdMissing}
}

func (c *Controller) SetCRDsMissing(missing bool) {
	c.mu.Lock()
	c.crdMissing = missing
	c.mu.Unlock()
}

func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return false
	}
	if dec.Decode(new(any)) != io.EOF {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return false
	}
	return true
}

// Routes mounts the agent endpoints. They are reachable only over the
// authenticated host channel, never by workloads.
func (c *Controller) Routes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /_agent/host/facts", func(w http.ResponseWriter, r *http.Request) {
		var f Facts
		if !decodeStrict(w, r, 64<<10, &f) {
			return
		}
		if err := c.SetFacts(f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_agent/host/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(c.Snapshot())
	})
	mux.HandleFunc("GET /_agent/host/requests", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"requests": c.Actionable()})
	})
	mux.HandleFunc("POST /_agent/host/decisions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Decisions []Decision `json:"decisions"`
		}
		if !decodeStrict(w, r, 64<<10, &body) || len(body.Decisions) > 64 {
			if len(body.Decisions) > 64 {
				http.Error(w, "Too many decisions", http.StatusBadRequest)
			}
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := c.SetDecisions(ctx, body.Decisions); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
