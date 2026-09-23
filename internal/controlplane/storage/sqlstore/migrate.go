// This is a minimal, dependency-free migration runner rather than an
// external tool like golang-migrate: this build environment's network
// egress policy blocks most non-github.com/package-registry hosts, and
// pulling in a full migration framework's dependency tree carries the same
// "will some transitive dependency need a host we can't reach" risk the
// golang.org/x/crypto work already had to route around once (see go.mod's
// replace directives). A flat, numbered .sql-file runner over a
// schema_migrations table is a well-understood, easy-to-audit pattern that
// needs nothing beyond database/sql — the same "avoid unnecessary
// dependencies" reasoning already applied elsewhere in this codebase
// (internal/agent/docker, internal/agent/hoststats).
package sqlstore

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrate applies every migration in migrations/ whose numeric prefix
// (e.g. "0001" in "0001_init.sql") is not yet recorded in
// schema_migrations, in ascending order, each in its own transaction so a
// failure partway through one file doesn't leave that file half-applied.
// Safe to call on every startup — an already-current database is a no-op.
// dialect selects placeholder rebinding for the one parameterized
// statement here (the schema_migrations bookkeeping INSERT); it runs
// before a *Store exists (Open calls this to build one), so it uses the
// standalone rebind() rather than Store.q.
func migrate(ctx context.Context, db *sql.DB, dialect string) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan applied migration version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	rows.Close()

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	type migration struct {
		version int
		name    string
	}
	var pending []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		versionStr, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return fmt.Errorf("migration file %q doesn't follow the NNNN_name.sql convention", e.Name())
		}
		version, err := strconv.Atoi(versionStr)
		if err != nil {
			return fmt.Errorf("migration file %q has a non-numeric version prefix: %w", e.Name(), err)
		}
		if applied[version] {
			continue
		}
		pending = append(pending, migration{version: version, name: e.Name()})
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })

	for _, m := range pending {
		sqlBytes, err := migrationFiles.ReadFile("migrations/" + m.name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", m.name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction for migration %s: %w", m.name, err)
		}
		// database/sql doesn't support multiple statements in one Exec
		// for every driver, so each migration file is split into
		// individual statements on ";" boundaries first. splitStatements
		// strips "--" line comments before splitting specifically so a
		// semicolon inside a comment can't masquerade as a statement
		// terminator — sufficient for this schema's straightforward DDL
		// (no stored procedures, and no statement containing a literal
		// ";" inside a string literal).
		for _, stmt := range splitStatements(string(sqlBytes)) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("apply migration %s: %w", m.name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			rebind(dialect, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`),
			m.version, time.Now().UTC().Format(timeLayout)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.name, err)
		}
	}
	return nil
}

// splitStatements strips "--" line comments (so an embedded ";" inside a
// comment can never be mistaken for a statement terminator), then splits
// the remaining SQL into individual non-empty statements on ";"
// boundaries. Not a full SQL tokenizer — a ";" inside a quoted string
// literal would still split incorrectly — but sufficient for this
// package's DDL-only migrations, none of which contain one.
func splitStatements(sqlText string) []string {
	var noComments strings.Builder
	for _, line := range strings.Split(sqlText, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		noComments.WriteString(line)
		noComments.WriteByte('\n')
	}

	var out []string
	for _, stmt := range strings.Split(noComments.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		out = append(out, stmt)
	}
	return out
}
