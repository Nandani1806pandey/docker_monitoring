// Historical metrics (ARCHITECTURE.md §12 / roadmap Phase B). See
// migrations/0002_historical_metrics.sql for the resolution/rollup
// schedule this file implements. HistoricalStore is additive on top of
// Store: it's a separate, optional interface (agentregistry.Registry
// treats it exactly like MetricBroadcaster — nil-safe, best-effort, never
// allowed to fail telemetry ingestion), not part of storage.Store, so the
// memory backend is never required to implement it — historical
// persistence only ever makes sense on top of real storage anyway.
package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/nandani/docker-monitor/internal/shared/models"
)

// bucketResolutionSeconds is the finest granularity ever written —
// see the migration file's header comment for why one minute.
const bucketResolutionSeconds = 60

// rollupAfter/rollupResolutionSeconds: buckets older than this are
// coarsened from 1-minute to 5-minute resolution by RunRetention.
const (
	rollupAfter             = 6 * time.Hour
	rollupResolutionSeconds = 300
)

// HistoricalPoint is one bucketed, possibly-averaged-over-many-samples
// reading returned by QueryRange. SampleCount and ResolutionSeconds are
// exposed so a chart can honestly represent "this point averages N raw
// samples over a 5-minute window" rather than implying single-sample
// precision it doesn't have.
type HistoricalPoint struct {
	BucketStart       time.Time `json:"bucket_start"`
	ResolutionSeconds int       `json:"resolution_seconds"`
	SampleCount       int       `json:"sample_count"`
	CPUPercentAvg     float64   `json:"cpu_percent_avg"`
	MemUsedBytesAvg   float64   `json:"mem_used_bytes_avg"`
	MemLimitBytesAvg  float64   `json:"mem_limit_bytes_avg"`
	NetRXBytesAvg     float64   `json:"net_rx_bytes_avg"`
	NetTXBytesAvg     float64   `json:"net_tx_bytes_avg"`
}

// RecordHistoricalSample folds m into its one-minute bucket via a
// running-average upsert (no read-modify-write round trip from Go —
// the arithmetic happens in the single UPSERT statement, referencing the
// pre-upsert row values by their unqualified column names and the
// proposed new row via the "excluded." prefix, which is valid syntax on
// both SQLite's and PostgreSQL's UPSERT and is the one and only reason
// this works unmodified on both dialects).
func (s *Store) RecordHistoricalSample(ctx context.Context, m *models.Metric) error {
	bucketStart := formatTime(m.CollectedAt.UTC().Truncate(time.Duration(bucketResolutionSeconds) * time.Second))

	query := `
		INSERT INTO metric_history (entity_type, entity_id, bucket_start, resolution_seconds,
			sample_count, cpu_percent_avg, mem_used_bytes_avg, mem_limit_bytes_avg,
			net_rx_bytes_avg, net_tx_bytes_avg)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT (entity_type, entity_id, resolution_seconds, bucket_start) DO UPDATE SET
			sample_count = sample_count + 1,
			cpu_percent_avg = cpu_percent_avg + (excluded.cpu_percent_avg - cpu_percent_avg) / (sample_count + 1),
			mem_used_bytes_avg = mem_used_bytes_avg + (excluded.mem_used_bytes_avg - mem_used_bytes_avg) / (sample_count + 1),
			mem_limit_bytes_avg = mem_limit_bytes_avg + (excluded.mem_limit_bytes_avg - mem_limit_bytes_avg) / (sample_count + 1),
			net_rx_bytes_avg = net_rx_bytes_avg + (excluded.net_rx_bytes_avg - net_rx_bytes_avg) / (sample_count + 1),
			net_tx_bytes_avg = net_tx_bytes_avg + (excluded.net_tx_bytes_avg - net_tx_bytes_avg) / (sample_count + 1)`

	_, err := s.db.ExecContext(ctx, s.q(query),
		m.EntityType, m.EntityID, bucketStart, bucketResolutionSeconds,
		m.CPUPercent, float64(m.MemUsedBytes), float64(m.MemLimitBytes),
		float64(m.NetRXBytes), float64(m.NetTXBytes),
	)
	if err != nil {
		return fmt.Errorf("record historical sample: %w", err)
	}
	return nil
}

// QueryRange returns every bucket for entityType/entityID whose
// bucket_start falls in [from, to], ascending by time. Because
// RunRetention deletes the finer-resolution rows it rolls up, a given
// period is stored at exactly one resolution at any point in time, so
// this is a single query with no client-side deduplication needed across
// overlapping resolutions.
func (s *Store) QueryRange(ctx context.Context, entityType, entityID string, from, to time.Time) ([]HistoricalPoint, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT bucket_start, resolution_seconds, sample_count, cpu_percent_avg,
			mem_used_bytes_avg, mem_limit_bytes_avg, net_rx_bytes_avg, net_tx_bytes_avg
		FROM metric_history
		WHERE entity_type = ? AND entity_id = ? AND bucket_start >= ? AND bucket_start <= ?
		ORDER BY bucket_start ASC`),
		entityType, entityID, formatTime(from), formatTime(to),
	)
	if err != nil {
		return nil, fmt.Errorf("query historical range: %w", err)
	}
	defer rows.Close()

	out := make([]HistoricalPoint, 0)
	for rows.Next() {
		var p HistoricalPoint
		var bucketStart string
		if err := rows.Scan(&bucketStart, &p.ResolutionSeconds, &p.SampleCount, &p.CPUPercentAvg,
			&p.MemUsedBytesAvg, &p.MemLimitBytesAvg, &p.NetRXBytesAvg, &p.NetTXBytesAvg); err != nil {
			return nil, fmt.Errorf("scan historical point: %w", err)
		}
		t, err := parseTime(bucketStart)
		if err != nil {
			return nil, fmt.Errorf("parse bucket_start: %w", err)
		}
		p.BucketStart = t
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query historical range: %w", err)
	}
	return out, nil
}

// RunRetention (1) rolls up 1-minute buckets older than rollupAfter into
// 5-minute buckets via a sample-count-weighted average, deleting the
// source rows, and (2) deletes anything (at any resolution) older than
// retentionDays. Safe to call repeatedly (e.g. on a periodic ticker) —
// each pass only touches buckets that have newly crossed the rollup/
// retention age thresholds. The weighted-average grouping is done in Go
// rather than SQL specifically to stay dialect-portable: bucketing an
// RFC3339 string timestamp into 5-minute windows needs different date
// functions in SQLite vs. PostgreSQL, whereas parsing the (already
// dialect-agnostic) stored strings and grouping in Go needs nothing
// dialect-specific at all.
func (s *Store) RunRetention(ctx context.Context, now time.Time, retentionDays int) error {
	if err := s.rollupOldMinuteBuckets(ctx, now); err != nil {
		return fmt.Errorf("rollup: %w", err)
	}
	if retentionDays > 0 {
		cutoff := formatTime(now.Add(-time.Duration(retentionDays) * 24 * time.Hour))
		if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM metric_history WHERE bucket_start < ?`), cutoff); err != nil {
			return fmt.Errorf("delete expired historical metrics: %w", err)
		}
	}
	return nil
}

func (s *Store) rollupOldMinuteBuckets(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-rollupAfter)

	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT entity_type, entity_id, bucket_start, sample_count, cpu_percent_avg,
			mem_used_bytes_avg, mem_limit_bytes_avg, net_rx_bytes_avg, net_tx_bytes_avg
		FROM metric_history WHERE resolution_seconds = ? AND bucket_start < ?`),
		bucketResolutionSeconds, formatTime(cutoff),
	)
	if err != nil {
		return fmt.Errorf("select buckets to roll up: %w", err)
	}

	type key struct {
		entityType, entityID, coarseBucket string
	}
	type agg struct {
		count                                               int
		cpuSum, memUsedSum, memLimitSum, netRXSum, netTXSum float64
	}
	groups := make(map[key]*agg)
	var sourceBuckets []string // exact bucket_start values consumed, for deletion

	for rows.Next() {
		var entityType, entityID, bucketStart string
		var count int
		var cpu, memUsed, memLimit, netRX, netTX float64
		if err := rows.Scan(&entityType, &entityID, &bucketStart, &count, &cpu, &memUsed, &memLimit, &netRX, &netTX); err != nil {
			rows.Close()
			return fmt.Errorf("scan bucket to roll up: %w", err)
		}
		t, err := parseTime(bucketStart)
		if err != nil {
			rows.Close()
			return fmt.Errorf("parse bucket_start during rollup: %w", err)
		}
		coarse := formatTime(t.Truncate(rollupResolutionSeconds * time.Second))
		k := key{entityType, entityID, coarse}
		g, ok := groups[k]
		if !ok {
			g = &agg{}
			groups[k] = g
		}
		// Sample-count-weighted sum: each source bucket already
		// averages `count` raw samples, so weight its average by that
		// count before summing, then divide by the total count once
		// all source buckets in this coarser window are folded in.
		g.count += count
		g.cpuSum += cpu * float64(count)
		g.memUsedSum += memUsed * float64(count)
		g.memLimitSum += memLimit * float64(count)
		g.netRXSum += netRX * float64(count)
		g.netTXSum += netTX * float64(count)
		sourceBuckets = append(sourceBuckets, bucketStart)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("select buckets to roll up: %w", err)
	}
	rows.Close()

	if len(groups) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rollup transaction: %w", err)
	}

	upsertQuery := s.q(`
		INSERT INTO metric_history (entity_type, entity_id, bucket_start, resolution_seconds,
			sample_count, cpu_percent_avg, mem_used_bytes_avg, mem_limit_bytes_avg,
			net_rx_bytes_avg, net_tx_bytes_avg)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (entity_type, entity_id, resolution_seconds, bucket_start) DO UPDATE SET
			sample_count = sample_count + excluded.sample_count,
			cpu_percent_avg = (cpu_percent_avg * sample_count + excluded.cpu_percent_avg * excluded.sample_count) / (sample_count + excluded.sample_count),
			mem_used_bytes_avg = (mem_used_bytes_avg * sample_count + excluded.mem_used_bytes_avg * excluded.sample_count) / (sample_count + excluded.sample_count),
			mem_limit_bytes_avg = (mem_limit_bytes_avg * sample_count + excluded.mem_limit_bytes_avg * excluded.sample_count) / (sample_count + excluded.sample_count),
			net_rx_bytes_avg = (net_rx_bytes_avg * sample_count + excluded.net_rx_bytes_avg * excluded.sample_count) / (sample_count + excluded.sample_count),
			net_tx_bytes_avg = (net_tx_bytes_avg * sample_count + excluded.net_tx_bytes_avg * excluded.sample_count) / (sample_count + excluded.sample_count)`)

	for k, g := range groups {
		_, err := tx.ExecContext(ctx, upsertQuery,
			k.entityType, k.entityID, k.coarseBucket, rollupResolutionSeconds, g.count,
			g.cpuSum/float64(g.count), g.memUsedSum/float64(g.count), g.memLimitSum/float64(g.count),
			g.netRXSum/float64(g.count), g.netTXSum/float64(g.count),
		)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("upsert rolled-up bucket: %w", err)
		}
	}

	deleteQuery := s.q(`DELETE FROM metric_history WHERE resolution_seconds = ? AND bucket_start = ?`)
	for _, b := range sourceBuckets {
		if _, err := tx.ExecContext(ctx, deleteQuery, bucketResolutionSeconds, b); err != nil {
			tx.Rollback()
			return fmt.Errorf("delete rolled-up source bucket: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollup transaction: %w", err)
	}
	return nil
}
