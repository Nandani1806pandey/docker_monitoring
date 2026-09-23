package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func (s *Store) AppendEvent(ctx context.Context, e *models.Event) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = timeNow()
	}
	var metadataJSON interface{}
	if len(e.Metadata) > 0 {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("marshal event metadata: %w", err)
		}
		metadataJSON = string(b)
	}
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO events (id, type, severity, host_id, container_id, user_id, message, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		e.ID, e.Type, string(e.Severity), e.HostID, e.ContainerID, e.UserID, e.Message,
		metadataJSON, formatTime(e.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

func (s *Store) ListEvents(ctx context.Context, filter storage.EventFilter) ([]*models.Event, error) {
	var where []string
	var args []interface{}

	if filter.HostID != nil {
		where = append(where, "host_id = ?")
		args = append(args, *filter.HostID)
	}
	if filter.ContainerID != nil {
		where = append(where, "container_id = ?")
		args = append(args, *filter.ContainerID)
	}
	if filter.Type != nil {
		where = append(where, "type = ?")
		args = append(args, *filter.Type)
	}
	if filter.Severity != nil {
		where = append(where, "severity = ?")
		args = append(args, string(*filter.Severity))
	}
	if filter.From != nil {
		where = append(where, "created_at >= ?")
		args = append(args, formatTime(*filter.From))
	}
	if filter.To != nil {
		where = append(where, "created_at <= ?")
		args = append(args, formatTime(*filter.To))
	}

	query := `SELECT id, type, severity, host_id, container_id, user_id, message, metadata, created_at FROM events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	out := make([]*models.Event, 0)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return out, nil
}

func scanEvent(row rowScanner) (*models.Event, error) {
	var e models.Event
	var severity, createdAt string
	var hostID, containerID, userID, metadata sql.NullString
	if err := row.Scan(&e.ID, &e.Type, &severity, &hostID, &containerID, &userID,
		&e.Message, &metadata, &createdAt); err != nil {
		return nil, err
	}
	e.Severity = models.EventSeverity(severity)
	if hostID.Valid {
		v := hostID.String
		e.HostID = &v
	}
	if containerID.Valid {
		v := containerID.String
		e.ContainerID = &v
	}
	if userID.Valid {
		v := userID.String
		e.UserID = &v
	}
	if metadata.Valid && metadata.String != "" {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(metadata.String), &m); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
		e.Metadata = m
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	e.CreatedAt = t
	return &e, nil
}
