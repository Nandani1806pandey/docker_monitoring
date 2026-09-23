// Package storage defines the repository interfaces the rest of the control
// plane codes against. Nothing outside this package (and its concrete
// implementations) may know whether data lives in memory, SQLite, or
// Postgres — that boundary is what keeps §32's storage abstraction real
// rather than aspirational.
package storage

import (
	"context"
	"errors"
	"time"

	"github.com/nandani/docker-monitor/internal/shared/models"
)

var ErrNotFound = errors.New("not found")

// HostStore manages Host records.
type HostStore interface {
	CreateHost(ctx context.Context, h *models.Host) error
	GetHost(ctx context.Context, id string) (*models.Host, error)
	ListHosts(ctx context.Context) ([]*models.Host, error)
	UpdateHost(ctx context.Context, h *models.Host) error
	DeleteHost(ctx context.Context, id string) error
}

// ContainerStore manages Container records.
type ContainerStore interface {
	UpsertContainer(ctx context.Context, c *models.Container) error
	GetContainer(ctx context.Context, id string) (*models.Container, error)
	ListContainersByHost(ctx context.Context, hostID string) ([]*models.Container, error)
	DeleteContainer(ctx context.Context, id string) error
}

// MetricStore manages ephemeral "recent" metrics. Historical/downsampled
// persistence (opt-in, §12) is a separate interface layered on top of this
// one in a later milestone — not implemented here.
type MetricStore interface {
	RecordMetric(ctx context.Context, m *models.Metric) error
	LatestMetric(ctx context.Context, entityType, entityID string) (*models.Metric, error)
}

// EventFilter narrows ListEvents queries (§E.1: /api/v1/events?host=&container=&type=&severity=&from=&to=).
type EventFilter struct {
	HostID      *string
	ContainerID *string
	Type        *string
	Severity    *models.EventSeverity
	From        *time.Time
	To          *time.Time
	Limit       int
}

// EventStore manages the append-only audit/event log. Events are never
// hard-deleted as a side effect of deleting their referenced host/container.
type EventStore interface {
	AppendEvent(ctx context.Context, e *models.Event) error
	ListEvents(ctx context.Context, filter EventFilter) ([]*models.Event, error)
}

// UserStore manages local User accounts (ARCHITECTURE.md §23, §G).
type UserStore interface {
	CreateUser(ctx context.Context, u *models.User) error
	GetUser(ctx context.Context, id string) (*models.User, error)
	// GetUserByEmail returns ErrNotFound if no user has that email.
	// Callers doing login MUST treat "not found" and "wrong password" as
	// producing the identical error/response — see auth.Service.Login —
	// so this method itself is fine to distinguish the cases internally.
	GetUserByEmail(ctx context.Context, email string) (*models.User, error)
	ListUsers(ctx context.Context) ([]*models.User, error)
	UpdateUser(ctx context.Context, u *models.User) error
	// CountUsers backs first-run bootstrap (create an initial admin only
	// when the store is genuinely empty, never overwriting an existing
	// account set).
	CountUsers(ctx context.Context) (int, error)
}

// SessionStore manages server-side login sessions (ARCHITECTURE.md §G —
// "the session record is the source of truth, enabling instant
// revocation", as opposed to a stateless JWT).
type SessionStore interface {
	CreateSession(ctx context.Context, s *models.Session) error
	// GetSessionByTokenHash returns ErrNotFound for both "never existed"
	// and "existed but was deleted" — callers can't distinguish a revoked
	// session from a bogus token, which is the point (no oracle for
	// enumerating valid-but-expired session IDs).
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*models.Session, error)
	DeleteSession(ctx context.Context, id string) error
	// DeleteSessionsForUser supports a future "log out everywhere"
	// action and is used by user deactivation flows once those exist.
	DeleteSessionsForUser(ctx context.Context, userID string) error
}

// Store is the aggregate interface the API layer depends on. Concrete
// backends (memory, sqlite, postgres) implement all of it.
type Store interface {
	HostStore
	ContainerStore
	MetricStore
	EventStore
	UserStore
	SessionStore

	// Close releases any underlying resources (DB connections, etc.).
	Close() error
}
