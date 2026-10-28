// Package hostapi is the generic bridge between the host launcher and workloads
// inside the cluster. The launcher pushes host facts to the guest agent; the agent
// publishes them as read-only host.containersanywhere.dev resources and surfaces
// approval-gated requests back to the launcher. Workloads never receive a host
// capability token and only the launcher UI approves anything.
package hostapi

import "time"

const (
	Group   = "host.containersanywhere.dev"
	Version = "v1alpha1"

	StatusResource        = "hoststatuses." + Group
	RequestResource       = "hostrequests." + Group
	RegistryTrustResource = "hostregistrytrusts." + Group
	SingletonName         = "host"

	// A fact older than this is reported as Unknown instead of being trusted.
	StaleAfter = 30 * time.Second
	// A request nobody approved within this window expires.
	RequestTTL = 24 * time.Hour
)

type Sizing struct {
	Allocated int64  `json:"allocated"`
	HostTotal int64  `json:"hostTotal"`
	Requested *int64 `json:"requested,omitempty"`
}

type Autostart struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Mechanism string `json:"mechanism,omitempty"`
	Note      string `json:"note,omitempty"`
}

type Update struct {
	// idle, prepared, activating, candidate-running, committed, cancelled, failed,
	// or the swap phases trying, committed, rolled-back.
	Phase                  string   `json:"phase"`
	Kind                   string   `json:"kind,omitempty"` // "transaction" or "swap"
	CandidateVersion       string   `json:"candidateVersion,omitempty"`
	AvailableVersion       string   `json:"availableVersion,omitempty"`
	BlockedReason          string   `json:"blockedReason,omitempty"`
	RetainedRecoveryPoints []string `json:"retainedRecoveryPoints,omitempty"`
}

type LANResult struct {
	Address   string `json:"address"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

type LANCheck struct {
	CheckedAt string      `json:"checkedAt"`
	Results   []LANResult `json:"results"`
	Note      string      `json:"note"`
}

type Gateway struct {
	Enabled                 bool      `json:"enabled"`
	RootCAURL               string    `json:"rootCAURL,omitempty"`
	RootCAFingerprintSHA256 string    `json:"rootCAFingerprintSHA256,omitempty"`
	HTTPPort                int       `json:"httpPort,omitempty"`
	HTTPSPort               int       `json:"httpsPort,omitempty"`
	BoundAddresses          []string  `json:"boundAddresses,omitempty"`
	FallbackPorts           bool      `json:"fallbackPorts"`
	LANCheck                *LANCheck `json:"lanCheck,omitempty"`
}

// Facts are what only the host can know. They contain no secrets and no host
// paths beyond the documented free-space figures.
type Facts struct {
	HostTime          string    `json:"hostTime"`
	AppVersion        string    `json:"appVersion"`
	ApplianceVersion  string    `json:"applianceVersion"`
	BundleID          string    `json:"bundleID"`
	K3sVersion        string    `json:"k3sVersion"`
	Arch              string    `json:"arch"`
	OS                string    `json:"os"`
	CPU               Sizing    `json:"cpu"`
	MemoryMiB         Sizing    `json:"memoryMiB"`
	StateDirFreeBytes int64     `json:"stateDirFreeBytes"`
	StateDirTotal     int64     `json:"stateDirTotalBytes"`
	LauncherHealthy   bool      `json:"launcherHealthy"`
	Autostart         Autostart `json:"autostart"`
	Update            Update    `json:"update"`
	Gateway           Gateway   `json:"gateway"`
	// Destinations are the backup destinations enrolled in the launcher (see HostDestination).
	Destinations   []Destination         `json:"destinations,omitempty"`
	RecoveryPoints []RecoveryPoint       `json:"recoveryPoints,omitempty"`
	BackupSchedule *BackupScheduleStatus `json:"backupSchedule,omitempty"`
	RecoveryKit    *RecoveryKit          `json:"recoveryKit,omitempty"`
	DataDisk       *DataDisk             `json:"dataDisk,omitempty"`
}

type Disk struct {
	SizeBytes      int64  `json:"sizeBytes"`
	UsedBytes      int64  `json:"usedBytes"`
	FreeBytes      int64  `json:"freeBytes"`
	Pressure       bool   `json:"pressure"`
	PressureReason string `json:"pressureReason,omitempty"`
}

type StorageUse struct {
	ImagesBytes  int64  `json:"imagesBytes"`
	VolumesBytes int64  `json:"volumesBytes"`
	ConsoleBytes int64  `json:"consoleBytes"`
	JournalBytes int64  `json:"journalBytes"`
	MeasuredAt   string `json:"measuredAt"`
	Partial      bool   `json:"partial,omitempty"`
}

type Logs struct {
	Persisted bool   `json:"persisted"`
	Bytes     int64  `json:"bytes"`
	CapBytes  int64  `json:"capBytes,omitempty"`
	Boots     int    `json:"boots"`
	Oldest    string `json:"oldest,omitempty"`
}

type GuestFacts struct {
	UptimeSeconds int64       `json:"uptimeSeconds"`
	Disk          Disk        `json:"disk"`
	Storage       *StorageUse `json:"storage,omitempty"`
	Logs          *Logs       `json:"logs,omitempty"`
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"` // True, False, Unknown
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// Status is the singleton HostStatus status. It reports truth: stale or missing
// host facts become Unknown, never silently the last known value.
type Status struct {
	ObservedAt          string      `json:"observedAt"`
	Host                *Facts      `json:"host,omitempty"`
	HostFactsAgeSeconds *int64      `json:"hostFactsAgeSeconds,omitempty"`
	ClockSkewSeconds    *float64    `json:"clockSkewSeconds,omitempty"`
	Guest               GuestFacts  `json:"guest"`
	Conditions          []Condition `json:"conditions"`
}

// DataDisk is the host's view of the cluster's data disk, so a manager can see real headroom
// before it asks for growth. The disk is a sparse file: SizeBytes is the ceiling the cluster
// sees and AllocatedBytes is what it really costs this computer today.
type DataDisk struct {
	SizeBytes      int64 `json:"sizeBytes"`
	AllocatedBytes int64 `json:"allocatedBytes"`
	// CeilingBytes is the largest the disk may be made. MaxGrowthBytes is how much more it
	// could grow right now: limited by the ceiling and by the host-free floor.
	CeilingBytes   int64 `json:"ceilingBytes"`
	FreeFloorBytes int64 `json:"hostFreeFloorBytes"`
	MaxGrowthBytes int64 `json:"maxGrowthBytes"`
}
