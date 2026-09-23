package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrate_IdempotentAndCreatesSchema(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "migrate.db")
	ctx := context.Background()

	s1, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s1.CountUsers(ctx); err != nil {
		t.Fatalf("schema not usable after first Open: %v", err)
	}
	s1.Close()

	s2, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("second Open (idempotent migrate): %v", err)
	}
	defer s2.Close()
	if _, err := s2.CountUsers(ctx); err != nil {
		t.Fatalf("schema not usable after second Open: %v", err)
	}
}

func TestHost_CRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	h := &models.Host{
		ID:               "host-1",
		Name:             "prod-01",
		Kind:             models.HostKindRemote,
		ConnectionStatus: models.ConnectionStatusOnline,
		MonitoringMode:   models.ModeNormal,
		LastHeartbeatAt:  &now,
		AgentEnrolled:    true,
		CertFingerprint:  "aa:bb:cc",
	}
	if err := s.CreateHost(ctx, h); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}

	got, err := s.GetHost(ctx, "host-1")
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if got.Name != "prod-01" || got.Kind != models.HostKindRemote || !got.AgentEnrolled {
		t.Fatalf("GetHost mismatch: %+v", got)
	}
	if got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(now) {
		t.Fatalf("LastHeartbeatAt mismatch: got %v want %v", got.LastHeartbeatAt, now)
	}

	got.ConnectionStatus = models.ConnectionStatusOffline
	if err := s.UpdateHost(ctx, got); err != nil {
		t.Fatalf("UpdateHost: %v", err)
	}
	got2, err := s.GetHost(ctx, "host-1")
	if err != nil {
		t.Fatalf("GetHost after update: %v", err)
	}
	if got2.ConnectionStatus != models.ConnectionStatusOffline {
		t.Fatalf("UpdateHost did not persist: %+v", got2)
	}

	list, err := s.ListHosts(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListHosts: %v, %d hosts", err, len(list))
	}

	if err := s.DeleteHost(ctx, "host-1"); err != nil {
		t.Fatalf("DeleteHost: %v", err)
	}
	if _, err := s.GetHost(ctx, "host-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := s.UpdateHost(ctx, got2); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound updating deleted host, got %v", err)
	}
	if err := s.DeleteHost(ctx, "host-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound deleting already-deleted host, got %v", err)
	}
}

func TestContainer_UpsertAndCascadeOnHostDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	h := &models.Host{ID: "host-1", Name: "h1", Kind: models.HostKindLocal,
		ConnectionStatus: models.ConnectionStatusOnline, MonitoringMode: models.ModeNormal}
	if err := s.CreateHost(ctx, h); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}

	c := &models.Container{
		ID: "c-1", HostID: "host-1", DockerContainerID: "docker123",
		Name: "web", Image: "nginx:latest", Status: models.ContainerStatusRunning,
		HealthStatus: models.HealthStatusHealthy, MonitoringMode: models.ModeNormal,
		Tags: map[string]string{"env": "prod"},
	}
	if err := s.UpsertContainer(ctx, c); err != nil {
		t.Fatalf("UpsertContainer (insert): %v", err)
	}

	c.Status = models.ContainerStatusExited
	c.RestartCount = 3
	if err := s.UpsertContainer(ctx, c); err != nil {
		t.Fatalf("UpsertContainer (update): %v", err)
	}

	got, err := s.GetContainer(ctx, "c-1")
	if err != nil {
		t.Fatalf("GetContainer: %v", err)
	}
	if got.Status != models.ContainerStatusExited || got.RestartCount != 3 {
		t.Fatalf("upsert did not update in place: %+v", got)
	}
	if got.Tags["env"] != "prod" {
		t.Fatalf("tags round-trip failed: %+v", got.Tags)
	}

	list, err := s.ListContainersByHost(ctx, "host-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListContainersByHost: %v, %d", err, len(list))
	}

	if err := s.DeleteHost(ctx, "host-1"); err != nil {
		t.Fatalf("DeleteHost: %v", err)
	}
	if _, err := s.GetContainer(ctx, "c-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected container cascade-deleted with host, got %v", err)
	}
}

func TestMetric_LatestOnlyOverwrite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.LatestMetric(ctx, "host", "host-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before any metric recorded, got %v", err)
	}

	m1 := &models.Metric{EntityType: "host", EntityID: "host-1", CPUPercent: 12.5,
		MemUsedBytes: 100, CollectedAt: time.Now().UTC(), Mode: models.ModeNormal, IntervalSeconds: 60}
	if err := s.RecordMetric(ctx, m1); err != nil {
		t.Fatalf("RecordMetric: %v", err)
	}

	m2 := &models.Metric{EntityType: "host", EntityID: "host-1", CPUPercent: 90.0,
		MemUsedBytes: 200, CollectedAt: time.Now().UTC(), Mode: models.ModeCritical, IntervalSeconds: 10}
	if err := s.RecordMetric(ctx, m2); err != nil {
		t.Fatalf("RecordMetric (overwrite): %v", err)
	}

	got, err := s.LatestMetric(ctx, "host", "host-1")
	if err != nil {
		t.Fatalf("LatestMetric: %v", err)
	}
	if got.CPUPercent != 90.0 || got.Mode != models.ModeCritical {
		t.Fatalf("expected latest-only overwrite, got %+v", got)
	}
}

func TestEvent_AppendAndFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	hostID := "host-1"
	h := &models.Host{ID: hostID, Name: "h1", Kind: models.HostKindLocal,
		ConnectionStatus: models.ConnectionStatusOnline, MonitoringMode: models.ModeNormal}
	if err := s.CreateHost(ctx, h); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}

	base := time.Now().UTC().Add(-time.Hour)
	ids := []string{"e-0", "e-1", "e-2"}
	for i, sev := range []models.EventSeverity{models.SeverityInfo, models.SeverityWarning, models.SeverityCritical} {
		e := &models.Event{
			ID: ids[i], Type: "container.status", Severity: sev,
			HostID: &hostID, Message: "test event", CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := s.AppendEvent(ctx, e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	all, err := s.ListEvents(ctx, storage.EventFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("ListEvents(no filter): %v, %d events", err, len(all))
	}
	if !all[0].CreatedAt.After(all[1].CreatedAt) {
		t.Fatalf("events not ordered newest-first: %+v", all)
	}

	sev := models.SeverityCritical
	filtered, err := s.ListEvents(ctx, storage.EventFilter{Severity: &sev})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("ListEvents(severity filter): %v, %d events", err, len(filtered))
	}

	limited, err := s.ListEvents(ctx, storage.EventFilter{Limit: 1})
	if err != nil || len(limited) != 1 {
		t.Fatalf("ListEvents(limit): %v, %d events", err, len(limited))
	}
}

func TestUserAndSession_CRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	u := &models.User{ID: "u-1", Email: "admin@example.com", DisplayName: "Admin",
		PasswordHash: "hash", Role: models.RoleAdmin}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if n, err := s.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("CountUsers: %v, %d", err, n)
	}
	got, err := s.GetUserByEmail(ctx, "admin@example.com")
	if err != nil || got.ID != "u-1" {
		t.Fatalf("GetUserByEmail: %v, %+v", err, got)
	}

	sess := &models.Session{ID: "s-1", UserID: "u-1", TokenHash: "tokhash",
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	gotSess, err := s.GetSessionByTokenHash(ctx, "tokhash")
	if err != nil || gotSess.UserID != "u-1" {
		t.Fatalf("GetSessionByTokenHash: %v, %+v", err, gotSess)
	}

	if err := s.DeleteSessionsForUser(ctx, "u-1"); err != nil {
		t.Fatalf("DeleteSessionsForUser: %v", err)
	}
	if _, err := s.GetSessionByTokenHash(ctx, "tokhash"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after DeleteSessionsForUser, got %v", err)
	}
}

func TestRestartPersistence(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "restart.db")
	ctx := context.Background()

	s1, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h := &models.Host{ID: "host-1", Name: "survives-restart", Kind: models.HostKindLocal,
		ConnectionStatus: models.ConnectionStatusOnline, MonitoringMode: models.ModeNormal}
	if err := s1.CreateHost(ctx, h); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("reopen after restart: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetHost(ctx, "host-1")
	if err != nil {
		t.Fatalf("host did not survive restart: %v", err)
	}
	if got.Name != "survives-restart" {
		t.Fatalf("unexpected host after restart: %+v", got)
	}
}
