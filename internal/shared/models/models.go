// Package models holds the domain types shared across the control plane.
// These are intentionally storage-agnostic: no DB tags, no ORM coupling.
package models

import "time"

type HostKind string

const (
	HostKindLocal  HostKind = "local"
	HostKindRemote HostKind = "remote"
)

type ConnectionStatus string

const (
	ConnectionStatusOnline  ConnectionStatus = "online"
	ConnectionStatusOffline ConnectionStatus = "offline"
	ConnectionStatusUnknown ConnectionStatus = "unknown"
)

// MonitoringMode mirrors the adaptive monitoring FSM states (see ARCHITECTURE.md §F).
type MonitoringMode string

const (
	ModeNormal         MonitoringMode = "normal"
	ModeCritical       MonitoringMode = "critical"
	ModeManualRealtime MonitoringMode = "manual_realtime"
)

// Host represents a monitored Docker host (local or remote), one per agent.
type Host struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Address          string           `json:"address,omitempty"`
	Kind             HostKind         `json:"kind"`
	AgentID          string           `json:"agent_id,omitempty"`
	ConnectionStatus ConnectionStatus `json:"connection_status"`
	DockerVersion    string           `json:"docker_version,omitempty"`
	OSInfo           string           `json:"os_info,omitempty"`
	MonitoringMode   MonitoringMode   `json:"monitoring_mode"`
	LastHeartbeatAt  *time.Time       `json:"last_heartbeat_at,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`

	// Agent enrollment/mTLS state (ARCHITECTURE.md §G, §45). The token
	// hash and issued-at are cleared once burned; never serialize the
	// plaintext token anywhere — it only ever exists in the API response
	// at issuance time (§25's "never display the secret again" pattern).
	EnrollmentTokenHash string     `json:"-"`
	EnrollmentIssuedAt  *time.Time `json:"-"`
	AgentEnrolled       bool       `json:"agent_enrolled"`
	CertFingerprint     string     `json:"cert_fingerprint,omitempty"`
}

type ContainerStatus string

const (
	ContainerStatusRunning ContainerStatus = "running"
	ContainerStatusExited  ContainerStatus = "exited"
	ContainerStatusPaused  ContainerStatus = "paused"
	ContainerStatusUnknown ContainerStatus = "unknown"
)

type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	HealthStatusNone      HealthStatus = "none"
)

// Container represents a Docker container discovered on a Host.
type Container struct {
	ID                string            `json:"id"`
	HostID            string            `json:"host_id"`
	DockerContainerID string            `json:"docker_container_id"`
	Name              string            `json:"name"`
	Image             string            `json:"image"`
	Status            ContainerStatus   `json:"status"`
	HealthStatus      HealthStatus      `json:"health_status"`
	RestartCount      int               `json:"restart_count"`
	MonitoringMode    MonitoringMode    `json:"monitoring_mode"`
	Tags              map[string]string `json:"tags,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	LastSeenAt        *time.Time        `json:"last_seen_at,omitempty"`
}

// Metric is a single point-in-time reading for a host or container, always
// carrying provenance so the UI can show freshness rather than implying
// stale data is live (see ARCHITECTURE.md §50).
type Metric struct {
	EntityType      string         `json:"entity_type"` // "host" | "container"
	EntityID        string         `json:"entity_id"`
	CPUPercent      float64        `json:"cpu_percent"`
	MemUsedBytes    uint64         `json:"mem_used_bytes"`
	MemLimitBytes   uint64         `json:"mem_limit_bytes,omitempty"`
	NetRXBytes      uint64         `json:"net_rx_bytes"`
	NetTXBytes      uint64         `json:"net_tx_bytes"`
	CollectedAt     time.Time      `json:"collected_at"`
	Mode            MonitoringMode `json:"mode"`
	IntervalSeconds int            `json:"interval_seconds"`
}

type EventSeverity string

const (
	SeverityInfo     EventSeverity = "info"
	SeverityWarning  EventSeverity = "warning"
	SeverityCritical EventSeverity = "critical"
)

// --- Users, sessions, and permissions (ARCHITECTURE.md §D, §G) ---
//
// This is a real, working first slice of §G's security model — Argon2id
// password hashing, server-side revocable sessions — rather than the full
// group/role/permission matrix §D's schema describes (groups, roles,
// permissions, role_permissions, group_roles). That full matrix is real
// future work, not implemented here: Role is a fixed two-value enum
// (RoleAdmin/RoleViewer) rather than an arbitrary set of named
// permissions, which covers "can this user mutate state" but not
// per-resource or per-capability grants (e.g. "can restart containers on
// host X but not host Y"). Documented as a deliberate scope boundary, the
// same way this codebase documents every other simplification, rather
// than silently passing off a two-role system as full RBAC.

// Role is a coarse-grained permission level. RoleAdmin can perform any
// mutating operation; RoleViewer can only read.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// User is a local (password-based) account. OIDC/SSO (§24) is not
// implemented — PasswordHash is always set for a User created by this
// build.
type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"display_name"`
	PasswordHash string     `json:"-"` // never serialized, including in API responses
	Role         Role       `json:"role"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// Session is a server-side login session (§G: "the session record — not a
// JWT — is the source of truth, enabling instant revocation"). Only
// TokenHash is ever persisted; the raw token is returned to the client
// exactly once, in the login response's cookie, and is never recoverable
// from stored state — the same handling API keys and enrollment tokens
// already get elsewhere in this codebase.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	TokenHash string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Event is an immutable audit/activity record. Events must never be deleted
// as a side effect of deleting the host/container they reference (see
// ARCHITECTURE.md §D — ON DELETE SET NULL, never CASCADE for audit data).
type Event struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"`
	Severity    EventSeverity          `json:"severity"`
	HostID      *string                `json:"host_id,omitempty"`
	ContainerID *string                `json:"container_id,omitempty"`
	UserID      *string                `json:"user_id,omitempty"`
	Message     string                 `json:"message"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt   time.Time              `json:"created_at"`
}
