// Package memory implements storage.Store with an in-process, mutex-guarded
// map-based store. It is the zero-dependency default: a single-host
// deployment can run the whole control plane with DM_STORAGE_DRIVER=memory
// and no database container at all. Data does not survive a restart —
// that trade-off is documented, not hidden.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

type Store struct {
	mu sync.RWMutex

	hosts      map[string]*models.Host
	containers map[string]*models.Container
	metrics    map[string]*models.Metric // key: entityType+"/"+entityID -> latest only
	events     []*models.Event
	users      map[string]*models.User
	sessions   map[string]*models.Session // key: session ID
}

func New() *Store {
	return &Store{
		hosts:      make(map[string]*models.Host),
		containers: make(map[string]*models.Container),
		metrics:    make(map[string]*models.Metric),
		users:      make(map[string]*models.User),
		sessions:   make(map[string]*models.Session),
	}
}

func (s *Store) Close() error { return nil }

// --- Hosts ---

func (s *Store) CreateHost(_ context.Context, h *models.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now().UTC()
	}
	cp := *h
	s.hosts[h.ID] = &cp
	return nil
}

func (s *Store) GetHost(_ context.Context, id string) (*models.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.hosts[id]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *h
	return &cp, nil
}

func (s *Store) ListHosts(_ context.Context) ([]*models.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*models.Host, 0, len(s.hosts))
	for _, h := range s.hosts {
		cp := *h
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) UpdateHost(_ context.Context, h *models.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.hosts[h.ID]; !ok {
		return storage.ErrNotFound
	}
	cp := *h
	s.hosts[h.ID] = &cp
	return nil
}

func (s *Store) DeleteHost(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.hosts[id]; !ok {
		return storage.ErrNotFound
	}
	delete(s.hosts, id)
	return nil
}

// --- Containers ---

func (s *Store) UpsertContainer(_ context.Context, c *models.Container) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	cp := *c
	s.containers[c.ID] = &cp
	return nil
}

func (s *Store) GetContainer(_ context.Context, id string) (*models.Container, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.containers[id]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (s *Store) ListContainersByHost(_ context.Context, hostID string) ([]*models.Container, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*models.Container, 0)
	for _, c := range s.containers {
		if c.HostID == hostID {
			cp := *c
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) DeleteContainer(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.containers[id]; !ok {
		return storage.ErrNotFound
	}
	delete(s.containers, id)
	return nil
}

// --- Metrics (latest-only cache; historical persistence is a separate,
// opt-in layer added in a later milestone) ---

func (s *Store) RecordMetric(_ context.Context, m *models.Metric) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics[m.EntityType+"/"+m.EntityID] = m
	return nil
}

func (s *Store) LatestMetric(_ context.Context, entityType, entityID string) (*models.Metric, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.metrics[entityType+"/"+entityID]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

// --- Events (append-only) ---

func (s *Store) AppendEvent(_ context.Context, e *models.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	cp := *e
	s.events = append(s.events, &cp)
	return nil
}

func (s *Store) ListEvents(_ context.Context, filter storage.EventFilter) ([]*models.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*models.Event, 0)
	for i := len(s.events) - 1; i >= 0; i-- { // newest first
		e := s.events[i]
		if filter.HostID != nil && (e.HostID == nil || *e.HostID != *filter.HostID) {
			continue
		}
		if filter.ContainerID != nil && (e.ContainerID == nil || *e.ContainerID != *filter.ContainerID) {
			continue
		}
		if filter.Type != nil && e.Type != *filter.Type {
			continue
		}
		if filter.Severity != nil && e.Severity != *filter.Severity {
			continue
		}
		if filter.From != nil && e.CreatedAt.Before(*filter.From) {
			continue
		}
		if filter.To != nil && e.CreatedAt.After(*filter.To) {
			continue
		}
		cp := *e
		out = append(out, &cp)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

var _ storage.Store = (*Store)(nil)

// --- Users ---

func (s *Store) CreateUser(_ context.Context, u *models.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	cp := *u
	s.users[u.ID] = &cp
	return nil
}

func (s *Store) GetUser(_ context.Context, id string) (*models.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *Store) GetUserByEmail(_ context.Context, email string) (*models.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, storage.ErrNotFound
}

func (s *Store) ListUsers(_ context.Context) ([]*models.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*models.User, 0, len(s.users))
	for _, u := range s.users {
		cp := *u
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) UpdateUser(_ context.Context, u *models.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[u.ID]; !ok {
		return storage.ErrNotFound
	}
	cp := *u
	s.users[u.ID] = &cp
	return nil
}

func (s *Store) CountUsers(_ context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users), nil
}

// --- Sessions ---

func (s *Store) CreateSession(_ context.Context, sess *models.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	cp := *sess
	s.sessions[sess.ID] = &cp
	return nil
}

func (s *Store) GetSessionByTokenHash(_ context.Context, tokenHash string) (*models.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.TokenHash == tokenHash {
			cp := *sess
			return &cp, nil
		}
	}
	return nil, storage.ErrNotFound
}

func (s *Store) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return storage.ErrNotFound
	}
	delete(s.sessions, id)
	return nil
}

func (s *Store) DeleteSessionsForUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.UserID == userID {
			delete(s.sessions, id)
		}
	}
	return nil
}
