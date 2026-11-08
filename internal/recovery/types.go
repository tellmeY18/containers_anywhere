// Package recovery implements cold, journaled system/full Btrfs recovery. Its
// catalog must reside on @snapshots, outside the system tree being restored.
package recovery

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/tellmeY18/containers_anywhere/internal/diskspace"
	"github.com/tellmeY18/containers_anywhere/internal/storage"
)

const Version = 1

var (
	ErrBusy         = errors.New("another recovery operation is pending")
	ErrInconsistent = errors.New("recovery is incomplete; K3s must not start")
	ErrCapacity     = errors.New("insufficient guest recovery headroom")
)

type Request struct {
	ID      string `json:"id,omitempty"` // Kubernetes UID; canonicalized to UID at submission
	UID     string `json:"uid"`
	Action  string `json:"action"` // capture or restore
	Scope   string `json:"scope"`  // system or full
	PointID string `json:"pointID,omitempty"`
}

type Operation struct {
	Version   int       `json:"version"`
	Request   Request   `json:"request"`
	Phase     string    `json:"phase"` // restart-needed, capturing, restoring, rolling-back, completed, failed
	PointID   string    `json:"pointID,omitempty"`
	SafetyID  string    `json:"safetyID,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Datastore struct {
	Kind           string            `json:"kind"`
	AdapterVersion int               `json:"adapterVersion"`
	K3s            string            `json:"k3s"`
	MappingSHA256  string            `json:"mappingSHA256"`
	Files          map[string]string `json:"files"` // database, WAL and node/server identity digests
}

// DatastoreAdapter is deliberately separate from filesystem capture. A future
// etcd adapter must define and validate its own coherent export/restore contract;
// naming etcd in a manifest does not enable it.
type DatastoreAdapter interface {
	Inspect(context.Context, string, string) (Datastore, error)
}

type Inventory struct {
	PoolPresent bool     `json:"poolPresent"`
	StateSHA256 string   `json:"stateSHA256,omitempty"`
	Volumes     []string `json:"volumes"`
	Snapshots   []string `json:"snapshots"`
}

type Manifest struct {
	Version          int       `json:"version"`
	ID               string    `json:"id"`
	Scope            string    `json:"scope"`
	CreatedAt        time.Time `json:"createdAt"`
	Appliance        string    `json:"appliance"`
	Architecture     string    `json:"architecture"`
	DataFormat       string    `json:"dataFormat"`
	Datastore        Datastore `json:"datastore"`
	Inventory        Inventory `json:"inventory"`
	Consistency      string    `json:"consistency"`
	GracefulShutdown bool      `json:"gracefulShutdown"`
	// Captures is the complete set of independently snapshotted subvolumes.
	Captures []string         `json:"captures"`
	Members  []SnapshotMember `json:"members,omitempty"`
}

// Backend.Sync makes preceding filesystem operations durable before publication.
// Receive accepts only a self-contained, validated stream and creates exactly
// the requested name.
type Backend interface {
	storage.FS
	Sync(context.Context, string) error
	ValidateLayout(context.Context, string, string) error
	Send(context.Context, string, io.Writer) error
	Receive(context.Context, string, string, io.Reader) error
	ReadOnly(context.Context, string) (bool, error)
	SetLimit(context.Context, string, int64) error
	Mounted(context.Context, string) (bool, error)
}

type Config struct {
	Root, VarLib, PoolDir                    string
	Appliance, Architecture, DataFormat, K3s string
	FS                                       Backend
	Datastore                                DatastoreAdapter
	Copy                                     storage.Copier
	// DiskSpace defaults to the attributed internal/diskspace implementation.
	// Tests inject filesystem pressure without querying the developer host.
	DiskSpace func(string) (diskspace.Usage, error)
	// Cold must verify that K3s/containerd cannot write. Only init-data should
	// provide this callback; serving processes must leave it nil.
	Cold func(context.Context) error
	Now  func() time.Time
}

type Engine struct{ cfg Config }
