package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

// RecordMetric overwrites the single latest-known reading for this
// entity — matching storage.MetricStore's documented scope exactly (see
// its interface doc comment: this is not a time series, just the most
// recent sample). Real historical/downsampled persistence is a separate
// interface layered on top of this one in a later milestone.
func (s *Store) RecordMetric(ctx context.Context, m *models.Metric) error {
	query := `
		INSERT INTO latest_metrics (entity_type, entity_id, cpu_percent, mem_used_bytes,
			mem_limit_bytes, net_rx_bytes, net_tx_bytes, collected_at, mode, interval_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (entity_type, entity_id) DO UPDATE SET
			cpu_percent=excluded.cpu_percent, mem_used_bytes=excluded.mem_used_bytes,
			mem_limit_bytes=excluded.mem_limit_bytes, net_rx_bytes=excluded.net_rx_bytes,
			net_tx_bytes=excluded.net_tx_bytes, collected_at=excluded.collected_at,
			mode=excluded.mode, interval_seconds=excluded.interval_seconds`
	_, err := s.db.ExecContext(ctx, s.q(query),
		m.EntityType, m.EntityID, m.CPUPercent, int64(m.MemUsedBytes), int64(m.MemLimitBytes),
		int64(m.NetRXBytes), int64(m.NetTXBytes), formatTime(m.CollectedAt), string(m.Mode), m.IntervalSeconds,
	)
	if err != nil {
		return fmt.Errorf("record metric: %w", err)
	}
	return nil
}

func (s *Store) LatestMetric(ctx context.Context, entityType, entityID string) (*models.Metric, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT entity_type, entity_id, cpu_percent, mem_used_bytes, mem_limit_bytes,
			net_rx_bytes, net_tx_bytes, collected_at, mode, interval_seconds
		FROM latest_metrics WHERE entity_type = ? AND entity_id = ?`), entityType, entityID)

	var m models.Metric
	var mode, collectedAt string
	var memUsed, memLimit, netRX, netTX int64
	if err := row.Scan(&m.EntityType, &m.EntityID, &m.CPUPercent, &memUsed, &memLimit,
		&netRX, &netTX, &collectedAt, &mode, &m.IntervalSeconds); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, storage.ErrNotFound
		}
		return nil, fmt.Errorf("get latest metric: %w", err)
	}
	m.MemUsedBytes = uint64(memUsed)
	m.MemLimitBytes = uint64(memLimit)
	m.NetRXBytes = uint64(netRX)
	m.NetTXBytes = uint64(netTX)
	m.Mode = models.MonitoringMode(mode)

	t, err := parseTime(collectedAt)
	if err != nil {
		return nil, fmt.Errorf("parse collected_at: %w", err)
	}
	m.CollectedAt = t

	return &m, nil
}
