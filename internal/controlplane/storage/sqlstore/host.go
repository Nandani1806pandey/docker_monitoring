package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func (s *Store) CreateHost(ctx context.Context, h *models.Host) error {
	if h.CreatedAt.IsZero() {
		h.CreatedAt = timeNow()
	}
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO hosts (id, name, address, kind, agent_id, connection_status,
			docker_version, os_info, monitoring_mode, last_heartbeat_at, created_at,
			enrollment_token_hash, enrollment_issued_at, agent_enrolled, cert_fingerprint)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		h.ID, h.Name, h.Address, string(h.Kind), h.AgentID, string(h.ConnectionStatus),
		h.DockerVersion, h.OSInfo, string(h.MonitoringMode), nullTime(h.LastHeartbeatAt), formatTime(h.CreatedAt),
		h.EnrollmentTokenHash, nullTime(h.EnrollmentIssuedAt), boolToInt(h.AgentEnrolled), h.CertFingerprint,
	)
	if err != nil {
		return fmt.Errorf("insert host: %w", err)
	}
	return nil
}

func (s *Store) GetHost(ctx context.Context, id string) (*models.Host, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, name, address, kind, agent_id, connection_status, docker_version, os_info,
			monitoring_mode, last_heartbeat_at, created_at, enrollment_token_hash,
			enrollment_issued_at, agent_enrolled, cert_fingerprint
		FROM hosts WHERE id = ?`), id)
	h, err := scanHost(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get host: %w", err)
	}
	return h, nil
}

func (s *Store) ListHosts(ctx context.Context) ([]*models.Host, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, name, address, kind, agent_id, connection_status, docker_version, os_info,
			monitoring_mode, last_heartbeat_at, created_at, enrollment_token_hash,
			enrollment_issued_at, agent_enrolled, cert_fingerprint
		FROM hosts ORDER BY created_at ASC`))
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer rows.Close()

	var out []*models.Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("scan host: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	return out, nil
}

func (s *Store) UpdateHost(ctx context.Context, h *models.Host) error {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE hosts SET name=?, address=?, kind=?, agent_id=?, connection_status=?,
			docker_version=?, os_info=?, monitoring_mode=?, last_heartbeat_at=?,
			enrollment_token_hash=?, enrollment_issued_at=?, agent_enrolled=?, cert_fingerprint=?
		WHERE id=?`),
		h.Name, h.Address, string(h.Kind), h.AgentID, string(h.ConnectionStatus),
		h.DockerVersion, h.OSInfo, string(h.MonitoringMode), nullTime(h.LastHeartbeatAt),
		h.EnrollmentTokenHash, nullTime(h.EnrollmentIssuedAt), boolToInt(h.AgentEnrolled), h.CertFingerprint,
		h.ID,
	)
	if err != nil {
		return fmt.Errorf("update host: %w", err)
	}
	return checkRowsAffected(res, "update host")
}

func (s *Store) DeleteHost(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM hosts WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete host: %w", err)
	}
	return checkRowsAffected(res, "delete host")
}

func scanHost(row rowScanner) (*models.Host, error) {
	var h models.Host
	var kind, connStatus, mode string
	var lastHeartbeat, enrollmentIssuedAt sql.NullString
	var createdAt string
	var agentEnrolled int
	if err := row.Scan(&h.ID, &h.Name, &h.Address, &kind, &h.AgentID, &connStatus,
		&h.DockerVersion, &h.OSInfo, &mode, &lastHeartbeat, &createdAt,
		&h.EnrollmentTokenHash, &enrollmentIssuedAt, &agentEnrolled, &h.CertFingerprint,
	); err != nil {
		return nil, err
	}
	h.Kind = models.HostKind(kind)
	h.ConnectionStatus = models.ConnectionStatus(connStatus)
	h.MonitoringMode = models.MonitoringMode(mode)
	h.AgentEnrolled = agentEnrolled != 0

	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	h.CreatedAt = t

	if lastHeartbeat.Valid {
		t, err := parseTime(lastHeartbeat.String)
		if err != nil {
			return nil, fmt.Errorf("parse last_heartbeat_at: %w", err)
		}
		h.LastHeartbeatAt = &t
	}
	if enrollmentIssuedAt.Valid {
		t, err := parseTime(enrollmentIssuedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse enrollment_issued_at: %w", err)
		}
		h.EnrollmentIssuedAt = &t
	}
	return &h, nil
}
