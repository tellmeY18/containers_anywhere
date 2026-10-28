package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

const DestinationResource = "hostdestinations." + Group

const maxDestinations = 16

var destinationID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Destination is the host's report on one launcher-enrolled backup destination.
// It carries a name the person chose and never a host path. Only the launcher UI
// enrols or removes destinations; workloads can only read this.
type Destination struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "folder"
	Available bool   `json:"available"`
	// SameDeviceAsData is true when the destination is on the same disk as the
	// cluster's data. Such a copy is not disaster recovery.
	SameDeviceAsData bool   `json:"sameDeviceAsData"`
	FreeBytes        int64  `json:"freeBytes"`
	TotalBytes       int64  `json:"totalBytes"`
	CheckedAt        string `json:"checkedAt"`
	LastError        string `json:"lastError,omitempty"`
}

func (d Destination) validate() error {
	if !destinationID.MatchString(d.ID) || !validString(d.Name) || d.Name == "" || !validString(d.Kind) ||
		!validString(d.CheckedAt) || !validString(d.LastError) || d.FreeBytes < 0 || d.TotalBytes < 0 {
		return errors.New("invalid destination")
	}
	return nil
}

func validateDestinations(list []Destination) error {
	if len(list) > maxDestinations {
		return errors.New("too many destinations")
	}
	seen := map[string]bool{}
	for _, d := range list {
		if err := d.validate(); err != nil || seen[d.ID] {
			return errors.New("invalid destination")
		}
		seen[d.ID] = true
	}
	return nil
}

func destinationKey(list []Destination) string {
	c := append([]Destination(nil), list...)
	for i := range c {
		c[i].CheckedAt = ""
	}
	b, _ := json.Marshal(c)
	return string(b)
}

type destinationStatus struct {
	Destination
	Conditions []Condition `json:"conditions"`
}

func destinationState(d Destination) destinationStatus {
	cond := Condition{Type: "Available", Status: "True", Reason: "Reachable", Message: "The destination can be written to"}
	if !d.Available {
		cond = Condition{Type: "Available", Status: "False", Reason: "Unavailable", Message: d.LastError}
	}
	return destinationStatus{Destination: d, Conditions: []Condition{cond}}
}

// ReconcileDestinations publishes one read-only HostDestination per enrolled
// destination and removes the ones that were un-enrolled. It does nothing while
// the host's facts are missing or stale, so an old picture is never published and
// a launcher restart does not delete anything.
func (c *Controller) ReconcileDestinations(ctx context.Context) error {
	c.mu.RLock()
	facts, at := c.facts, c.factsAt
	c.mu.RUnlock()
	now := c.now()
	if facts == nil || now.Sub(at) > StaleAfter {
		return nil
	}
	key := destinationKey(facts.Destinations)
	c.mu.Lock()
	skip := c.destWritten && key == c.destKey && now.Sub(c.destWrite) < 30*time.Second
	c.mu.Unlock()
	if skip {
		return nil
	}
	var objs []publishedObject
	for _, d := range facts.Destinations {
		objs = append(objs, publishedObject{Name: d.ID, Status: destinationState(d)})
	}
	if err := c.reconcileObjects(ctx, DestinationResource, "HostDestination", objs); err != nil {
		return err
	}
	c.mu.Lock()
	c.destWritten, c.destKey, c.destWrite = true, key, now
	c.mu.Unlock()
	return nil
}

type publishedObject struct {
	Name   string
	Status any
}

// reconcileObjects makes the cluster-scoped, read-only objects of one kind match what the
// host reported: it applies each, writes its status, and deletes the ones that are gone.
func (c *Controller) reconcileObjects(ctx context.Context, resource, kind string, objs []publishedObject) error {
	want := map[string]bool{}
	for _, o := range objs {
		want[o.Name] = true
		if _, err := c.Run(ctx, objectApply(kind, o.Name), "apply", "--server-side", "--field-manager=containers-host-api", "-f", "-"); err != nil {
			return err
		}
		patch, _ := json.Marshal([]map[string]any{{"op": "add", "path": "/status", "value": o.Status}})
		if _, err := c.Run(ctx, nil, "patch", resource, o.Name, "--subresource=status", "--type=json", "-p", string(patch)); err != nil {
			return err
		}
	}
	out, err := c.Run(ctx, nil, "get", resource, "-o", "json")
	if err != nil {
		return err
	}
	var list struct {
		Items []struct {
			Metadata Meta `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return err
	}
	for _, it := range list.Items {
		if !want[it.Metadata.Name] {
			if _, err := c.Run(ctx, nil, "delete", resource, it.Metadata.Name, "--ignore-not-found"); err != nil {
				return err
			}
		}
	}
	return nil
}

func recoveryKey(list []RecoveryPoint) string {
	c := append([]RecoveryPoint(nil), list...)
	b, _ := json.Marshal(c)
	return string(b)
}
