-- Migration 0002: historical metrics (ARCHITECTURE.md §12 / roadmap Phase
-- B), layered alongside latest_metrics rather than replacing it —
-- storage.MetricStore's latest-only cache serves live-dashboard reads,
-- this table serves range/chart queries, and the two have different
-- write/read patterns that don't belong in one table.
--
-- Deliberate resolution schedule (documented here since it's an
-- implementation decision, not something obvious from the code): the
-- finest granularity ever stored is one-minute buckets
-- (resolution_seconds=60), written directly at ingestion time via a
-- running-average upsert (see historical_metric.go) rather than storing
-- every individual raw sample — NORMAL-mode telemetry already arrives at
-- ~60s intervals, so a 1-minute bucket is at or near raw resolution during
-- normal operation, and legitimately downsamples the higher-frequency
-- CRITICAL (10s) and MANUAL_REALTIME (1-2s) telemetry into something a
-- historical chart can use without ballooning storage every time a host
-- goes into an elevated polling mode. Buckets older than 6 hours are
-- rolled up into 5-minute buckets (resolution_seconds=300) by a periodic
-- retention job; anything older than the configured retention window is
-- deleted outright. At any point in time a given entity/period has rows
-- at exactly one resolution — the retention job deletes the finer-grained
-- rows it just rolled up, so range queries never need to deduplicate
-- overlapping resolutions themselves.
CREATE TABLE IF NOT EXISTS metric_history (
    entity_type TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    bucket_start TEXT NOT NULL,
    resolution_seconds INTEGER NOT NULL,
    sample_count INTEGER NOT NULL,
    cpu_percent_avg REAL NOT NULL,
    mem_used_bytes_avg REAL NOT NULL,
    mem_limit_bytes_avg REAL NOT NULL,
    net_rx_bytes_avg REAL NOT NULL,
    net_tx_bytes_avg REAL NOT NULL,
    PRIMARY KEY (entity_type, entity_id, resolution_seconds, bucket_start)
);
CREATE INDEX IF NOT EXISTS idx_metric_history_lookup ON metric_history(entity_type, entity_id, bucket_start);
