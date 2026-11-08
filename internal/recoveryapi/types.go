// Package recoveryapi reconciles app-delivered recovery CRDs. The host owns the
// journal; Kubernetes status is only a projection and is never an execution log.
package recoveryapi

import (
	"context"
	"encoding/json"
	"time"
)

const (
	Group                  = "recovery.containersanywhere.dev"
	Version                = "v1alpha1"
	WorkloadServiceAccount = "recovery-workload"
	SnapshotClass          = "recovery-btrfs"
	MaxMembers             = 64
)

type Runner func(context.Context, []byte, ...string) ([]byte, error)

type Metadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
}

type Selector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

// Hooks are explicit pod/container commands, never shell-expanded by the
// controller. Resume must be idempotent and Check must fail while still quiesced.
type Hook struct {
	Pod            string   `json:"pod"`
	Container      string   `json:"container"`
	Prepare        []string `json:"prepare"`
	Resume         []string `json:"resume"`
	Check          []string `json:"check"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
}

type ConfigRef struct {
	Kind string `json:"kind"` // ConfigMap only; no implicit secrets or operator state.
	Name string `json:"name"`
}

type CaptureSpec struct {
	Selector        Selector    `json:"selector"`
	ExpectedPVCs    []string    `json:"expectedPVCs"`
	Consistency     string      `json:"consistency"` // PerPVC or QuiescedGroup; never atomic.
	Hooks           []Hook      `json:"hooks,omitempty"`
	ConfigResources []ConfigRef `json:"configResources,omitempty"`
	TimeoutSeconds  int         `json:"timeoutSeconds"`
}

type PointSpec struct {
	Scope       string       `json:"scope,omitempty"`
	Capture     *CaptureSpec `json:"capture,omitempty"`
	PolicyUID   string       `json:"policyUID,omitempty"`
	ScheduledAt *time.Time   `json:"scheduledAt,omitempty"`
	Destination string       `json:"destination,omitempty"`
}

type RestoreSpec struct {
	Scope            string            `json:"scope,omitempty"`
	RecoveryPointID  string            `json:"recoveryPointID,omitempty"`
	RecoveryPoint    string            `json:"recoveryPoint,omitempty"`
	RecoveryPointUID string            `json:"recoveryPointUID,omitempty"`
	DurablePointID   string            `json:"durablePointID,omitempty"`
	PVCNames         map[string]string `json:"pvcNames,omitempty"`
}

// ImportedRestoreRequest confers namespace-scoped authority only. The adapter
// validates source inventory membership and creates fresh, empty request-owned
// targets using the supplied fixed service account, never node-admin exec.
type ImportedRestoreRequest struct {
	RequestUID     string            `json:"requestUID"`
	Namespace      string            `json:"namespace"`
	NamespaceUID   string            `json:"namespaceUID"`
	ServiceAccount string            `json:"serviceAccount"`
	PointID        string            `json:"pointID"`
	PVCNames       map[string]string `json:"pvcNames"`
}

type ImportedRestoreStatus struct {
	Phase        string            `json:"phase"` // Pending, Running, Succeeded, Failed
	Reason       string            `json:"reason,omitempty"`
	Message      string            `json:"message,omitempty"`
	RestoredPVCs map[string]string `json:"restoredPVCs,omitempty"`
}

type PolicySpec struct {
	Scope       string       `json:"scope,omitempty"`
	Schedule    string       `json:"schedule"` // numeric five-field cron, UTC only
	Suspend     bool         `json:"suspend,omitempty"`
	KeepLast    int          `json:"keepLast"`
	Capture     *CaptureSpec `json:"capture,omitempty"`
	Destination string       `json:"destination,omitempty"`
}

type Object struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   Metadata        `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Status     *Status         `json:"status,omitempty"`
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type DestinationSpec struct {
	Provider             string     `json:"provider"`
	AllowedNamespaces    []string   `json:"allowedNamespaces,omitempty"`
	HostDirectoryID      string     `json:"hostDirectoryID,omitempty"`
	Endpoint             string     `json:"endpoint,omitempty"`
	Bucket               string     `json:"bucket,omitempty"`
	Prefix               string     `json:"prefix,omitempty"`
	CredentialsSecretRef *SecretRef `json:"credentialsSecretRef,omitempty"`
}

type SecretRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type DestinationStatus struct {
	Conditions []Condition `json:"conditions"`
}

type Destination struct {
	Name string          `json:"name"`
	UID  string          `json:"uid"`
	Spec DestinationSpec `json:"spec"`
}

// Export is separate from local capture. The adapter must deduplicate by point
// UID + destination UID, validate registered CSI identities, authenticate the
// completed archive, and only then return Succeeded. No credentials in status.
type ExportStatus struct {
	Phase   string `json:"phase"` // Pending, Running, Succeeded, Failed
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Receipt string `json:"receipt,omitempty"` // opaque, no host paths or secrets
}

// Backend is the authenticated guest-engine adapter. Submit MUST durably dedupe
// UID, including after a disk rewind. Get returns the same immutable operation.
// Cluster permissions are supplied only by cluster-scoped CRs, never namespaced
// objects. Backend owns cold-capture/restore, compatibility and power sequencing.
type Backend interface {
	Submit(context.Context, ClusterOperation) (OperationStatus, error)
	Get(context.Context, string) (OperationStatus, error)
}

type ClusterOperation struct {
	Scope           string `json:"scope"` // system or full; backend verifies source manifest on restore
	UID             string `json:"uid"`
	Action          string `json:"action"` // Capture or Restore
	RecoveryPointID string `json:"recoveryPointID,omitempty"`
	Destination     string `json:"destination,omitempty"`
}

type OperationStatus struct {
	Phase           string `json:"phase"` // Pending, Running, RestartRequired, Succeeded, Failed
	Reason          string `json:"reason,omitempty"`
	Message         string `json:"message,omitempty"`
	RecoveryPointID string `json:"recoveryPointID,omitempty"`
}

type Member struct {
	PVC            string   `json:"pvc"`
	UID            string   `json:"uid"`
	Snapshot       string   `json:"snapshot"`
	SnapshotUID    string   `json:"snapshotUID,omitempty"`
	Content        string   `json:"content,omitempty"`
	ContentUID     string   `json:"contentUID,omitempty"`
	SnapshotHandle string   `json:"snapshotHandle,omitempty"`
	VolumeName     string   `json:"volumeName"`
	VolumeHandle   string   `json:"volumeHandle,omitempty"`
	StorageClass   string   `json:"storageClass"`
	Storage        string   `json:"storage"`
	AccessModes    []string `json:"accessModes"`
	VolumeMode     string   `json:"volumeMode,omitempty"`
	Ready          bool     `json:"ready"`
}

type HookAttempt struct {
	Hook    Hook   `json:"hook"`
	PodUID  string `json:"podUID"`
	Resumed bool   `json:"resumed"`
}

// Record is the independent catalog and preparation/resume journal. Records
// have no Kubernetes owner reference and are retained after namespace deletion.
type Record struct {
	Scope             string                     `json:"scope,omitempty"`
	UID               string                     `json:"uid"`
	Kind              string                     `json:"kind"`
	Name              string                     `json:"name"`
	Namespace         string                     `json:"namespace,omitempty"`
	NamespaceUID      string                     `json:"namespaceUID,omitempty"`
	SpecHash          string                     `json:"specHash"`
	PolicyUID         string                     `json:"policyUID,omitempty"`
	Phase             string                     `json:"phase"`
	Reason            string                     `json:"reason,omitempty"`
	Message           string                     `json:"message,omitempty"`
	CreatedAt         time.Time                  `json:"createdAt"`
	CompletedAt       *time.Time                 `json:"completedAt,omitempty"`
	Consistency       string                     `json:"consistency,omitempty"`
	Members           []Member                   `json:"members,omitempty"`
	Hooks             []HookAttempt              `json:"hooks,omitempty"`
	Config            map[string]json.RawMessage `json:"config,omitempty"`
	RestoredPVCs      map[string]string          `json:"restoredPVCs,omitempty"`
	ProtectedBy       []string                   `json:"protectedBy,omitempty"`
	ExportPending     bool                       `json:"exportPending,omitempty"`
	ExportReason      string                     `json:"exportReason,omitempty"`
	Destination       string                     `json:"destination,omitempty"`
	DestinationUID    string                     `json:"destinationUID,omitempty"`
	Export            ExportStatus               `json:"export"`
	BackendSubmitted  bool                       `json:"backendSubmitted,omitempty"`
	BackendAttempted  bool                       `json:"backendAttempted,omitempty"`
	Operation         *ClusterOperation          `json:"operation,omitempty"`
	RecoveryPointID   string                     `json:"recoveryPointID,omitempty"`
	LastSchedule      *time.Time                 `json:"lastSchedule,omitempty"`
	PendingSchedule   *time.Time                 `json:"pendingSchedule,omitempty"`
	AwaitingCapture   string                     `json:"awaitingCapture,omitempty"`
	RetentionEligible bool                       `json:"retentionEligible,omitempty"`
	Prune             *PruneState                `json:"prune,omitempty"`
	ResumeUncertain   bool                       `json:"resumeUncertain,omitempty"`
}

type PruneState struct {
	StartedAt   time.Time              `json:"startedAt"`
	BackendDone bool                   `json:"backendDone"`
	Members     map[string]PruneMember `json:"members,omitempty"`
}

type PruneMember struct {
	DeleteArmed bool `json:"deleteArmed"`
	Complete    bool `json:"complete"`
}

type Status struct {
	ObservedGeneration int64             `json:"observedGeneration"`
	Phase              string            `json:"phase"`
	Reason             string            `json:"reason,omitempty"`
	Message            string            `json:"message,omitempty"`
	Consistency        string            `json:"consistency,omitempty"`
	Members            []Member          `json:"members,omitempty"`
	RestoredPVCs       map[string]string `json:"restoredPVCs,omitempty"`
	RecoveryPointID    string            `json:"recoveryPointID,omitempty"`
	ExportPending      bool              `json:"exportPending,omitempty"`
	ExportReason       string            `json:"exportReason,omitempty"`
	Export             ExportStatus      `json:"export"`
	LastSchedule       *time.Time        `json:"lastSchedule,omitempty"`
	Conditions         []Condition       `json:"conditions,omitempty"`
}
