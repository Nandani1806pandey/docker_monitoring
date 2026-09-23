package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func (s *Store) UpsertContainer(ctx context.Context, c *models.Container) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = timeNow()
	}
	tagsJSON, err := marshalTags(c.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	query := `
		INSERT INTO containers (id, host_id, docker_container_id, name, image, status,
			health_status, restart_count, monitoring_mode, tags, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			host_id=excluded.host_id, docker_container_id=excluded.docker_container_id,
			name=excluded.name, image=excluded.image, status=excluded.status,
			health_status=excluded.health_status, restart_count=excluded.restart_count,
			monitoring_mode=excluded.monitoring_mode, tags=excluded.tags,
			last_seen_at=excluded.last_seen_at`
	_, err = s.db.ExecContext(ctx, s.q(query),
		c.ID, c.HostID, c.DockerContainerID, c.Name, c.Image, string(c.Status),
		string(c.HealthStatus), c.RestartCount, string(c.MonitoringMode), tagsJSON,
		formatTime(c.CreatedAt), nullTime(c.LastSeenAt),
	)
	if err != nil {
		return fmt.Errorf("upsert container: %w", err)
	}
	return nil
}

func (s *Store) GetContainer(ctx context.Context, id string) (*models.Container, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, host_id, docker_container_id, name, image, status, health_status,
			restart_count, monitoring_mode, tags, created_at, last_seen_at
		FROM containers WHERE id = ?`), id)
	c, err := scanContainer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get container: %w", err)
	}
	return c, nil
}

func (s *Store) ListContainersByHost(ctx context.Context, hostID string) ([]*models.Container, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, host_id, docker_container_id, name, image, status, health_status,
			restart_count, monitoring_mode, tags, created_at, last_seen_at
		FROM containers WHERE host_id = ? ORDER BY name ASC`), hostID)
	if err != nil {
		return nil, fmt.Errorf("list containers by host: %w", err)
	}
	defer rows.Close()

	var out []*models.Container
	for rows.Next() {
		c, err := scanContainer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan container: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list containers by host: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteContainer(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM containers WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete container: %w", err)
	}
	return checkRowsAffected(res, "delete container")
}

func marshalTags(tags map[string]string) (interface{}, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func scanContainer(row rowScanner) (*models.Container, error) {
	var c models.Container
	var status, health, mode string
	var tags, lastSeenAt sql.NullString
	var createdAt string
	if err := row.Scan(&c.ID, &c.HostID, &c.DockerContainerID, &c.Name, &c.Image, &status,
		&health, &c.RestartCount, &mode, &tags, &createdAt, &lastSeenAt); err != nil {
		return nil, err
	}
	c.Status = models.ContainerStatus(status)
	c.HealthStatus = models.HealthStatus(health)
	c.MonitoringMode = models.MonitoringMode(mode)

	if tags.Valid && tags.String != "" {
		var m map[string]string
		if err := json.Unmarshal([]byte(tags.String), &m); err != nil {
			return nil, fmt.Errorf("unmarshal tags: %w", err)
		}
		c.Tags = m
	}

	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	c.CreatedAt = t

	if lastSeenAt.Valid {
		t, err := parseTime(lastSeenAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse last_seen_at: %w", err)
		}
		c.LastSeenAt = &t
	}
	return &c, nil
}
