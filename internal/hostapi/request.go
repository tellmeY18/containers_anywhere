package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	TypeResources   = "resources"
	TypePower       = "power"
	TypeAutostart   = "autostart"
	TypePruneImages = "prune-images"
	TypeLANCheck    = "lan-check"

	PhasePending  = "Pending"
	PhaseApproved = "Approved"
	PhaseRejected = "Rejected"
	PhaseApplied  = "Applied"
	PhaseFailed   = "Failed"
	PhaseExpired  = "Expired"

	MinMemoryMiB = 2048
	MaxMemoryMiB = 256 << 10
	MinCPUs      = 2
	MaxCPUs      = 64
)

type Meta struct {
	Name              string  `json:"name"`
	Namespace         string  `json:"namespace"`
	UID               string  `json:"uid"`
	ResourceVersion   string  `json:"resourceVersion"`
	Generation        int64   `json:"generation"`
	CreationTimestamp string  `json:"creationTimestamp"`
	DeletionTimestamp *string `json:"deletionTimestamp,omitempty"`
}

type ResourcesSpec struct {
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memoryMiB"`
}

type PowerSpec struct {
	Action string `json:"action"`
}

type AutostartSpec struct {
	Enabled bool `json:"enabled"`
}

type RequestSpec struct {
	Type      string         `json:"type"`
	Reason    string         `json:"reason,omitempty"`
	Resources *ResourcesSpec `json:"resources,omitempty"`
	Power     *PowerSpec     `json:"power,omitempty"`
	Autostart *AutostartSpec `json:"autostart,omitempty"`
}

type RequestStatus struct {
	Phase              string          `json:"phase,omitempty"`
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	Reason             string          `json:"reason,omitempty"`
	Message            string          `json:"message,omitempty"`
	UpdatedAt          string          `json:"updatedAt,omitempty"`
	Result             json.RawMessage `json:"result,omitempty"`
}

type Request struct {
	Metadata Meta          `json:"metadata"`
	Spec     RequestSpec   `json:"spec"`
	Status   RequestStatus `json:"status"`
}

// Validate accepts exactly the parameters for the declared type and nothing else.
func (s RequestSpec) Validate() error {
	if !validString(s.Reason) {
		return errors.New("invalid reason")
	}
	set := 0
	for _, present := range []bool{s.Resources != nil, s.Power != nil, s.Autostart != nil} {
		if present {
			set++
		}
	}
	switch s.Type {
	case TypeResources:
		if s.Resources == nil || set != 1 {
			return errors.New("resources requests carry exactly the resources parameters")
		}
		if s.Resources.CPUs < MinCPUs || s.Resources.CPUs > MaxCPUs || s.Resources.MemoryMiB < MinMemoryMiB || s.Resources.MemoryMiB > MaxMemoryMiB {
			return fmt.Errorf("cpus must be %d-%d and memoryMiB %d-%d", MinCPUs, MaxCPUs, MinMemoryMiB, MaxMemoryMiB)
		}
	case TypePower:
		if s.Power == nil || set != 1 {
			return errors.New("power requests carry exactly the power parameters")
		}
		if s.Power.Action != "shutdown" && s.Power.Action != "restart-appliance" {
			return errors.New("power action must be shutdown or restart-appliance")
		}
	case TypeAutostart:
		if s.Autostart == nil || set != 1 {
			return errors.New("autostart requests carry exactly the autostart parameters")
		}
	case TypePruneImages, TypeLANCheck:
		if set != 0 {
			return fmt.Errorf("%s requests take no parameters", s.Type)
		}
	default:
		return fmt.Errorf("unknown request type %q", s.Type)
	}
	return nil
}

func terminal(phase string) bool {
	switch phase {
	case PhaseRejected, PhaseApplied, PhaseFailed, PhaseExpired:
		return true
	}
	return false
}

// CanTransition is the whole state machine. Terminal phases never change, so a
// decision can never be replayed onto a request that already completed.
func CanTransition(from, to string) bool {
	switch from {
	case "":
		return to == PhasePending || to == PhaseRejected
	case PhasePending:
		return to == PhaseApproved || to == PhaseRejected || to == PhaseApplied || to == PhaseFailed || to == PhaseExpired
	case PhaseApproved:
		return to == PhaseApplied || to == PhaseFailed
	}
	return false
}

// Decision is what the launcher reports back about a request.
type Decision struct {
	UID     string          `json:"uid"`
	Phase   string          `json:"phase"`
	Reason  string          `json:"reason,omitempty"`
	Message string          `json:"message,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

func (d Decision) Validate() error {
	if d.UID == "" || !validString(d.UID) || !validString(d.Reason) || len(d.Message) > 1024 || len(d.Result) > 1024 {
		return errors.New("invalid decision")
	}
	switch d.Phase {
	case PhaseApproved, PhaseRejected, PhaseApplied, PhaseFailed:
	default:
		return errors.New("a decision must be Approved, Rejected, Applied or Failed")
	}
	if len(d.Result) > 0 && !json.Valid(d.Result) {
		return errors.New("result must be JSON")
	}
	return nil
}

// Actionable is the slice of a request the launcher needs in order to ask a human.
type Actionable struct {
	UID        string      `json:"uid"`
	Namespace  string      `json:"namespace"`
	Name       string      `json:"name"`
	Generation int64       `json:"generation"`
	Created    string      `json:"created"`
	Phase      string      `json:"phase"`
	Spec       RequestSpec `json:"spec"`
}

func (r Request) expired(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, r.Metadata.CreationTimestamp)
	return err == nil && now.Sub(t) > RequestTTL
}
