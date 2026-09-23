// Package sqlstore implements storage.Store on top of database/sql,
// supporting both SQLite (single-host, zero-extra-infra deployments,
// ARCHITECTURE.md §B) and PostgreSQL (multi-user/scale deployments) behind
// one identical code path. The only per-dialect difference anywhere in
// this package is bind-parameter syntax (`?` vs `$1`) — see rebind below —
// everything else (schema, queries, error semantics) is shared, which is
// the actual point of storage.Store as an abstraction: callers in
// internal/controlplane/api never know or care which of memory/sqlite/
// postgres they're talking to.
//
// Error semantics deliberately mirror internal/controlplane/storage/memory
// exactly (storage.ErrNotFound on missing rows, zero-rows-affected on
// Update/Delete treated as ErrNotFound) so the API layer's error handling
// doesn't change based on which backend is configured.
package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"

	_ "github.com/lib/pq"           // postgres driver, registered as "postgres"
	_ "github.com/mattn/go-sqlite3" // sqlite driver, registered as "sqlite3"
)

// timeLayout is fixed-width RFC3339 UTC (always 9 fractional digits, always
// "Z", never a numeric offset) specifically so that lexicographic string
// ordering of stored timestamps is identical to chronological ordering in
// both SQLite and Postgres — see migrations/0001_init.sql's header comment.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func timeNow() time.Time { return time.Now().UTC() }

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t, nil
}

// nullTime formats an optional *time.Time as a nullable string parameter:
// nil stays NULL, a set value is formatted the same way formatTime does.
func nullTime(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkRowsAffected turns "the UPDATE/DELETE matched zero rows" into
// storage.ErrNotFound, matching the memory backend's behavior exactly
// (an update/delete against a nonexistent ID is a not-found, not a
// silent no-op).
func checkRowsAffected(res sql.Result, verb string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", verb, err)
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting single-row
// scan helpers (scanUser, scanHost, ...) serve both GetX and ListX.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

// Store implements storage.Store against a database/sql handle. Which
// dialect ("sqlite" or "postgres") it was opened with only ever affects
// rebind's placeholder translation — every query, scan, and error path in
// this package is otherwise dialect-agnostic.
type Store struct {
	db      *sql.DB
	dialect string
}

var _ storage.Store = (*Store)(nil)

// Open opens (creating the SQLite file if needed) or connects to (Postgres)
// the database at dsn, applies any pending migrations, and returns a ready
// Store. driver is "sqlite"/"sqlite3" or "postgres"/"postgresql".
func Open(ctx context.Context, driver, dsn string) (*Store, error) {
	var driverName, dialect string
	switch driver {
	case "sqlite", "sqlite3":
		driverName, dialect = "sqlite3", "sqlite"
	case "postgres", "postgresql":
		driverName, dialect = "postgres", "postgres"
	default:
		return nil, fmt.Errorf("sqlstore: unsupported driver %q (want \"sqlite\" or \"postgres\")", driver)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: open %s: %w", driver, err)
	}

	if dialect == "sqlite" {
		// SQLite serializes writers at the file level regardless of how
		// many *database/sql* connections are open; capping the pool at 1
		// turns "database is locked" errors under concurrent writers into
		// ordinary connection-pool queuing instead. WAL lets readers
		// proceed without blocking on an in-flight writer.
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlstore: enable WAL: %w", err)
		}
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlstore: enable foreign_keys: %w", err)
		}
	} else {
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(5 * time.Minute)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlstore: ping %s: %w", driver, err)
	}

	if err := migrate(ctx, db, dialect); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlstore: migrate: %w", err)
	}

	return &Store{db: db, dialect: dialect}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// q rebinds a query written with `?` placeholders (the style every query
// in this package is written in) to the target dialect's actual syntax.
// SQLite accepts `?` natively, so this is a no-op there; Postgres requires
// `$1, $2, ...`, so on that dialect every `?` is rewritten in left-to-right
// order. This is the one and only place placeholder-syntax divergence
// lives — see the package doc comment.
func (s *Store) q(query string) string { return rebind(s.dialect, query) }

// rebind is the standalone form of the same translation, usable by migrate()
// (which runs before a *Store exists — see Open) as well as by Store.q.
func rebind(dialect, query string) string {
	if dialect != "postgres" {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
