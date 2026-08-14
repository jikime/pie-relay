package control

import "time"

const (
	KindUser              = "users"
	KindNode              = "nodes"
	KindDevice            = "devices"
	KindRuntime           = "runtimes"
	KindSession           = "sessions"
	KindParticipant       = "participants"
	KindGrant             = "grants"
	KindOperation         = "operations"
	KindAudit             = "audit"
	KindIntegration       = "integrations"
	KindIntegrationSecret = "integration-secrets"
	KindIntegrationUser   = "integration-users"
	KindProject           = "projects"
	KindPreview           = "previews"
	KindConversation      = "conversations"
)

type CredentialProfile struct {
	TargetPath string `json:"targetPath"`
	Format     string `json:"format"`
	MaxBytes   int64  `json:"maxBytes"`
}

type Integration struct {
	ID                      string            `json:"id"`
	DisplayName             string            `json:"displayName"`
	Status                  string            `json:"status"`
	Credential              CredentialProfile `json:"credential"`
	MaxUsers                int               `json:"maxUsers"`
	MaxProjectsPerUser      int               `json:"maxProjectsPerUser"`
	MaxPreviewsPerUser      int               `json:"maxPreviewsPerUser"`
	MaxConversationsPerUser int               `json:"maxConversationsPerUser"`
	CreatedAt               time.Time         `json:"createdAt"`
	UpdatedAt               time.Time         `json:"updatedAt"`
	Version                 int64             `json:"version"`
}

// IntegrationSecret is intentionally excluded from Snapshot and all list
// endpoints. Only the constant-time service-token verifier reads it.
type IntegrationSecret struct {
	ID        string    `json:"id"`
	TokenHash string    `json:"tokenHash"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Version   int64     `json:"version"`
}

type IntegrationUser struct {
	ID                  string    `json:"id"`
	IntegrationID       string    `json:"integrationId"`
	ExternalUserID      string    `json:"externalUserId"`
	OwnerUserID         string    `json:"ownerUserId"`
	Status              string    `json:"status"`
	CredentialVersion   int64     `json:"credentialVersion,omitempty"`
	CredentialDigest    string    `json:"credentialDigest,omitempty"`
	CredentialUpdatedAt time.Time `json:"credentialUpdatedAt,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
	Version             int64     `json:"version"`
}

// Project is a server-owned directory inside one Integration user's Executor.
// Name is display-only; WorkingDir is derived from the opaque ID so untrusted
// project names can never influence a container path.
type Project struct {
	ID                string     `json:"id"`
	IntegrationID     string     `json:"integrationId"`
	IntegrationUserID string     `json:"integrationUserId"`
	OwnerUserID       string     `json:"ownerUserId"`
	Name              string     `json:"name"`
	Locale            string     `json:"locale"`
	WorkingDir        string     `json:"workingDir"`
	PreviewAppPath    string     `json:"previewAppPath,omitempty"`
	Status            string     `json:"status"`
	LastError         string     `json:"lastError,omitempty"`
	InitializedAt     *time.Time `json:"initializedAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	Version           int64      `json:"version"`
}

// Preview maps one opaque public hostname to a server-managed process inside
// the owning user's Executor. BackendHost is a Docker-network alias generated
// by the trusted runtime; callers can never supply an arbitrary proxy target.
type Preview struct {
	ID                string `json:"id"`
	IntegrationID     string `json:"integrationId"`
	IntegrationUserID string `json:"integrationUserId"`
	OwnerUserID       string `json:"ownerUserId"`
	ProjectID         string `json:"projectId"`
	AppPath           string `json:"appPath"`
	Hostname          string `json:"hostname"`
	BackendHost       string `json:"backendHost,omitempty"`
	Port              int    `json:"port"`
	Profile           string `json:"profile"`
	Visibility        string `json:"visibility"`
	// AccessVersion is advanced whenever the preview access policy changes.
	// Launch links and private session cookies are bound to this generation so
	// an old private session cannot survive a public/private policy change.
	AccessVersion int64      `json:"accessVersion,omitempty"`
	Status        string     `json:"status"`
	LastError     string     `json:"lastError,omitempty"`
	StartAttempts int        `json:"startAttempts,omitempty"`
	NextRetryAt   *time.Time `json:"nextRetryAt,omitempty"`
	LastReadyAt   *time.Time `json:"lastReadyAt,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	Version       int64      `json:"version"`
}

type Conversation struct {
	ID                string    `json:"id"`
	IntegrationID     string    `json:"integrationId"`
	IntegrationUserID string    `json:"integrationUserId"`
	OwnerUserID       string    `json:"ownerUserId"`
	DeviceID          string    `json:"deviceId"`
	ProjectID         string    `json:"projectId,omitempty"`
	SessionID         string    `json:"sessionId"`
	AgentSessionID    string    `json:"agentSessionId,omitempty"`
	Status            string    `json:"status"`
	LastSequence      uint64    `json:"lastSequence,omitempty"`
	LastError         string    `json:"lastError,omitempty"`
	LastActivityAt    time.Time `json:"lastActivityAt,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	Version           int64     `json:"version"`
}

type ResourceQuota struct {
	CPUs            string `json:"cpus,omitempty"`
	MemoryBytes     int64  `json:"memoryBytes,omitempty"`
	PIDs            int64  `json:"pids,omitempty"`
	DiskBytes       int64  `json:"diskBytes,omitempty"`
	MaxSessions     int    `json:"maxSessions,omitempty"`
	MaxParticipants int    `json:"maxParticipants,omitempty"`
}

type User struct {
	ID                  string        `json:"id"`
	ExternalSubject     string        `json:"externalSubject,omitempty"`
	OrganizationID      string        `json:"organizationId,omitempty"`
	Status              string        `json:"status"`
	Quota               ResourceQuota `json:"quota"`
	LifecycleEventID    string        `json:"lifecycleEventId,omitempty"`
	LifecycleEventHash  string        `json:"lifecycleEventHash,omitempty"`
	LifecycleOccurredAt time.Time     `json:"lifecycleOccurredAt,omitempty"`
	CreatedAt           time.Time     `json:"createdAt"`
	UpdatedAt           time.Time     `json:"updatedAt"`
	Version             int64         `json:"version"`
}

type NodeCapacity struct {
	CPUCores       float64 `json:"cpuCores,omitempty"`
	MemoryBytes    int64   `json:"memoryBytes,omitempty"`
	DiskBytes      int64   `json:"diskBytes,omitempty"`
	MaxConnections int     `json:"maxConnections,omitempty"`
}

type NodeUsage struct {
	CPUPercent  float64 `json:"cpuPercent,omitempty"`
	MemoryBytes int64   `json:"memoryBytes,omitempty"`
	DiskBytes   int64   `json:"diskBytes,omitempty"`
	Instances   int     `json:"instances,omitempty"`
	Connections int     `json:"connections,omitempty"`
}

type Node struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	// Address is the public HTTP(S) origin returned to Relay participants and
	// isolated Docker executors. ControlAddress is the private/internal origin
	// used only by Manager-side operations. Keeping them separate prevents an
	// external client from receiving a Docker-only hostname such as `relay`.
	Address             string            `json:"address,omitempty"`
	ControlAddress      string            `json:"controlAddress,omitempty"`
	PoolID              string            `json:"poolId,omitempty"`
	Region              string            `json:"region,omitempty"`
	AllowedApplications []string          `json:"allowedApplications,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	Capacity            NodeCapacity      `json:"capacity"`
	Usage               NodeUsage         `json:"usage"`
	LastHeartbeat       time.Time         `json:"lastHeartbeat,omitempty"`
	CreatedAt           time.Time         `json:"createdAt"`
	UpdatedAt           time.Time         `json:"updatedAt"`
	Version             int64             `json:"version"`
}

type Device struct {
	ID              string            `json:"id"`
	OwnerUserID     string            `json:"ownerUserId"`
	Name            string            `json:"name"`
	Kind            string            `json:"kind"`
	RuntimeID       string            `json:"runtimeId,omitempty"`
	DesiredState    string            `json:"desiredState"`
	ObservedState   string            `json:"observedState"`
	RuntimeRunning  bool              `json:"runtimeRunning"`
	RuntimeHealthy  bool              `json:"runtimeHealthy"`
	ClientConnected bool              `json:"clientConnected"`
	RelayRegistered bool              `json:"relayRegistered"`
	RelayNodeID     string            `json:"relayNodeId,omitempty"`
	ActiveSessions  int               `json:"activeSessions"`
	LastHeartbeat   time.Time         `json:"lastHeartbeat,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	CreatedAt       time.Time         `json:"createdAt"`
	UpdatedAt       time.Time         `json:"updatedAt"`
	Version         int64             `json:"version"`
}

type RuntimeInstance struct {
	ID             string        `json:"id"`
	DeviceID       string        `json:"deviceId"`
	OwnerUserID    string        `json:"ownerUserId"`
	NodeID         string        `json:"nodeId"`
	ContainerID    string        `json:"containerId,omitempty"`
	Image          string        `json:"image"`
	ImageVersion   string        `json:"imageVersion,omitempty"`
	DesiredState   string        `json:"desiredState"`
	ObservedState  string        `json:"observedState"`
	Health         string        `json:"health,omitempty"`
	Quota          ResourceQuota `json:"quota"`
	WorkspaceRef   string        `json:"workspaceRef,omitempty"`
	AuthVolumeRef  string        `json:"authVolumeRef,omitempty"`
	LastReconciled time.Time     `json:"lastReconciled,omitempty"`
	LastError      string        `json:"lastError,omitempty"`
	CreatedAt      time.Time     `json:"createdAt"`
	UpdatedAt      time.Time     `json:"updatedAt"`
	Version        int64         `json:"version"`
}

type Session struct {
	ID                string `json:"id"`
	OwnerUserID       string `json:"ownerUserId"`
	DeviceID          string `json:"deviceId"`
	ProjectID         string `json:"projectId,omitempty"`
	ApplicationID     string `json:"applicationId,omitempty"`
	PoolID            string `json:"poolId,omitempty"`
	TenantID          string `json:"tenantId,omitempty"`
	ResourceType      string `json:"resourceType,omitempty"`
	ResourceID        string `json:"resourceId,omitempty"`
	AgentID           string `json:"agentId,omitempty"`
	Protocol          string `json:"protocol,omitempty"`
	Name              string `json:"name,omitempty"`
	ExecutionTarget   string `json:"executionTarget"`
	AgentMode         string `json:"agentMode,omitempty"`
	AccessMode        string `json:"accessMode"`
	TransportMode     string `json:"transportMode"`
	SelectedTransport string `json:"selectedTransport,omitempty"`
	Status            string `json:"status"`
	RelayNodeID       string `json:"relayNodeId,omitempty"`
	// RelayGeneration is a server-managed fencing epoch. Reassignment bumps it,
	// and Relay includes it in the verified routing key so stale credentials
	// cannot enter a recovered session.
	RelayGeneration      int64      `json:"relayGeneration"`
	HostConnectionID     string     `json:"hostConnectionId,omitempty"`
	DriverUserID         string     `json:"driverUserId,omitempty"`
	DriverLeaseExpiresAt *time.Time `json:"driverLeaseExpiresAt,omitempty"`
	LastActivityAt       time.Time  `json:"lastActivityAt,omitempty"`
	StartAttempts        int        `json:"startAttempts,omitempty"`
	LastError            string     `json:"lastError,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
	UpdatedAt            time.Time  `json:"updatedAt"`
	Version              int64      `json:"version"`
}

type Participant struct {
	ID           string    `json:"id"`
	SessionID    string    `json:"sessionId"`
	UserID       string    `json:"userId"`
	ConnectionID string    `json:"connectionId"`
	Role         string    `json:"role"`
	Access       string    `json:"access"`
	Transport    string    `json:"transport,omitempty"`
	ConnectedAt  time.Time `json:"connectedAt"`
	LastSeenAt   time.Time `json:"lastSeenAt"`
	Version      int64     `json:"version"`
}

type AccessGrant struct {
	ID             string     `json:"id"`
	OwnerUserID    string     `json:"ownerUserId"`
	SubjectUserID  string     `json:"subjectUserId"`
	TargetDeviceID string     `json:"targetDeviceId"`
	SessionID      string     `json:"sessionId,omitempty"`
	Access         string     `json:"access"`
	Capabilities   []string   `json:"capabilities"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	Version        int64      `json:"version"`
}

type Operation struct {
	ID             string         `json:"id"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
	ActorUserID    string         `json:"actorUserId"`
	Type           string         `json:"type"`
	TargetKind     string         `json:"targetKind"`
	TargetID       string         `json:"targetId"`
	Status         string         `json:"status"`
	Progress       int            `json:"progress"`
	Request        map[string]any `json:"request,omitempty"`
	Result         map[string]any `json:"result,omitempty"`
	Error          string         `json:"error,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	StartedAt      *time.Time     `json:"startedAt,omitempty"`
	FinishedAt     *time.Time     `json:"finishedAt,omitempty"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	Version        int64          `json:"version"`
}

type AuditEvent struct {
	ID          string         `json:"id"`
	ActorUserID string         `json:"actorUserId"`
	Action      string         `json:"action"`
	TargetKind  string         `json:"targetKind"`
	TargetID    string         `json:"targetId"`
	RequestID   string         `json:"requestId,omitempty"`
	SourceIP    string         `json:"sourceIp,omitempty"`
	Success     bool           `json:"success"`
	Reason      string         `json:"reason,omitempty"`
	Before      map[string]any `json:"before,omitempty"`
	After       map[string]any `json:"after,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	Version     int64          `json:"version"`
}

type Snapshot struct {
	Users            []User            `json:"users"`
	Nodes            []Node            `json:"nodes"`
	Devices          []Device          `json:"devices"`
	Runtimes         []RuntimeInstance `json:"runtimes"`
	Sessions         []Session         `json:"sessions"`
	Participants     []Participant     `json:"participants"`
	Grants           []AccessGrant     `json:"grants"`
	Operations       []Operation       `json:"operations"`
	Audit            []AuditEvent      `json:"audit"`
	Integrations     []Integration     `json:"integrations"`
	IntegrationUsers []IntegrationUser `json:"integrationUsers"`
	Projects         []Project         `json:"projects"`
	Previews         []Preview         `json:"previews"`
	Conversations    []Conversation    `json:"conversations"`
}

type Overview struct {
	Users                   int            `json:"users"`
	OnlineDevices           int            `json:"onlineDevices"`
	RunningRuntimes         int            `json:"runningRuntimes"`
	ActiveSessions          int            `json:"activeSessions"`
	Participants            int            `json:"participants"`
	RelayConnections        int            `json:"relayConnections"`
	Integrations            int            `json:"integrations"`
	IntegrationUsers        int            `json:"integrationUsers"`
	Projects                int            `json:"projects"`
	Previews                int            `json:"previews"`
	ActiveConversations     int            `json:"activeConversations"`
	IntegrationUsersByState map[string]int `json:"integrationUsersByState"`
	ProjectsByState         map[string]int `json:"projectsByState"`
	PreviewsByState         map[string]int `json:"previewsByState"`
	ConversationsByState    map[string]int `json:"conversationsByState"`
	OperationsByState       map[string]int `json:"operationsByState"`
	DevicesByState          map[string]int `json:"devicesByState"`
	SessionsByState         map[string]int `json:"sessionsByState"`
	GeneratedAt             time.Time      `json:"generatedAt"`
}

type Event struct {
	Sequence uint64    `json:"sequence"`
	Kind     string    `json:"kind"`
	Action   string    `json:"action"`
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
}

type RelayPresence struct {
	EventID         string    `json:"eventId"`
	NodeID          string    `json:"nodeId"`
	ApplicationID   string    `json:"applicationId,omitempty"`
	PoolID          string    `json:"poolId,omitempty"`
	PublicURL       string    `json:"publicUrl,omitempty"`
	ControlURL      string    `json:"controlUrl,omitempty"`
	Room            string    `json:"room"`
	DeviceID        string    `json:"deviceId,omitempty"`
	SessionID       string    `json:"sessionId,omitempty"`
	RelayGeneration int64     `json:"relayGeneration,omitempty"`
	UserID          string    `json:"userId"`
	Role            string    `json:"role"`
	Access          string    `json:"access,omitempty"`
	ConnectionID    string    `json:"connectionId"`
	Kind            string    `json:"kind"` // host | participant
	Connected       bool      `json:"connected"`
	HostOnline      bool      `json:"hostOnline"`
	Heartbeat       bool      `json:"heartbeat,omitempty"`
	At              time.Time `json:"at"`
}
