-- Migration 0001: initial schema for everything storage.Store's interfaces
-- currently need (Host, Container, latest-only Metric, Event, User,
-- Session). Deliberately does NOT create tables for features that don't
-- have Go models yet (alerts, notification providers, groups/roles/
-- permissions, API keys, audit logs) — those land with their own
-- migration in the roadmap phase that actually implements them, so schema
-- and code land together instead of empty tables drifting from a design
-- that hasn't been built yet.
--
-- Deliberately dialect-portable: only TEXT/INTEGER/REAL column types are
-- used (both SQLite and PostgreSQL accept these as standard types),
-- timestamps are stored as fixed-width RFC3339 UTC strings (so
-- lexicographic ordering == chronological ordering in both engines, no
-- native TIMESTAMP type needed), and booleans are stored as INTEGER 0/1.
-- This lets one identical .sql file run unmodified on both backends.
-- `ON CONFLICT ... DO UPDATE` syntax used elsewhere in this package is
-- identical between SQLite 3.24+ and PostgreSQL, so upserts don't need
-- per-dialect SQL either — the only actual dialect difference in this
-- whole package is placeholder syntax (`?` vs `$1`), handled by rebind(),
-- not schema.

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL,
    created_at TEXT NOT NULL,
    last_login_at TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);

CREATE TABLE IF NOT EXISTS hosts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    address TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    agent_id TEXT NOT NULL DEFAULT '',
    connection_status TEXT NOT NULL,
    docker_version TEXT NOT NULL DEFAULT '',
    os_info TEXT NOT NULL DEFAULT '',
    monitoring_mode TEXT NOT NULL,
    last_heartbeat_at TEXT,
    created_at TEXT NOT NULL,
    enrollment_token_hash TEXT NOT NULL DEFAULT '',
    enrollment_issued_at TEXT,
    agent_enrolled INTEGER NOT NULL DEFAULT 0,
    cert_fingerprint TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS containers (
    id TEXT PRIMARY KEY,
    host_id TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    docker_container_id TEXT NOT NULL,
    name TEXT NOT NULL,
    image TEXT NOT NULL,
    status TEXT NOT NULL,
    health_status TEXT NOT NULL,
    restart_count INTEGER NOT NULL DEFAULT 0,
    monitoring_mode TEXT NOT NULL,
    tags TEXT,
    created_at TEXT NOT NULL,
    last_seen_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_containers_host_id ON containers(host_id);

-- Latest-only metric cache, matching storage.MetricStore's current scope
-- exactly (one row per entity, overwritten on every RecordMetric — no
-- history here; a separate historical-metrics interface/table lands later
-- without touching this one).
CREATE TABLE IF NOT EXISTS latest_metrics (
    entity_type TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    cpu_percent REAL NOT NULL,
    mem_used_bytes INTEGER NOT NULL,
    mem_limit_bytes INTEGER NOT NULL DEFAULT 0,
    net_rx_bytes INTEGER NOT NULL,
    net_tx_bytes INTEGER NOT NULL,
    collected_at TEXT NOT NULL,
    mode TEXT NOT NULL,
    interval_seconds INTEGER NOT NULL,
    PRIMARY KEY (entity_type, entity_id)
);

-- Events use ON DELETE SET NULL, never CASCADE, on their host/container
-- references — audit/operational history must survive the deletion of
-- what it references.
CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    severity TEXT NOT NULL,
    host_id TEXT REFERENCES hosts(id) ON DELETE SET NULL,
    container_id TEXT REFERENCES containers(id) ON DELETE SET NULL,
    user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    message TEXT NOT NULL,
    metadata TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_host_id ON events(host_id);
CREATE INDEX IF NOT EXISTS idx_events_container_id ON events(container_id);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(type);
CREATE INDEX IF NOT EXISTS idx_events_severity ON events(severity);
CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at);
