// Copyright 2026 The MCPDoll Authors.

package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed all:migrations
var migrationFS embed.FS

// migrationLockKey identifies MCPDoll's advisory lock.
//
// Advisory locks share one namespace per database, so the constant is arbitrary
// but must not collide with another application's. Derived from "mcpdoll" so it
// is reproducible rather than a number somebody picked.
const migrationLockKey int64 = 0x6d6370646f6c6c // "mcpdoll" in ASCII

// Migrate applies every migration that has not run yet, in filename order.
//
// Deliberately minimal — no external migration tool. A migration runner has one
// job, the failure modes are well understood, and adding a binary that has to
// be installed before the stack can start is a real cost for a project whose
// whole point is that `make up` works.
//
// Each migration runs inside a transaction together with the row recording it,
// so a migration and the record of having applied it cannot disagree. A partial
// migration that the ledger claims succeeded is the failure this prevents.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// One session at a time, across every process.
	//
	// `CREATE TABLE IF NOT EXISTS` is not safe under concurrency: two sessions
	// both pass the existence check and one fails on a duplicate pg_type entry.
	// Two control-plane replicas starting together hit this, and so does a
	// parallel test suite — which is how it was found.
	//
	// A session-level advisory lock is held for the whole run, so a replica
	// that arrives mid-migration waits rather than racing.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquiring a connection for migration: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("store: taking the migration lock: %w", err)
	}
	defer func() {
		// Best effort: releasing the connection drops a session-level lock
		// anyway, so a failure here cannot strand it.
		_, _ = conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("store: creating the migration ledger: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = $1)`, name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("store: checking migration %q: %w", name, err)
		}
		if applied {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: reading migration %q: %w", name, err)
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("store: beginning migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: applying migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: recording migration %q: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: committing migration %q: %w", name, err)
		}
	}
	return nil
}

// migrationNames lists the embedded migrations in the order they must run.
//
// Sorted by filename, which is why they are numbered. A migration that must run
// after another and does not sort after it is a bug the numbering prevents.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: reading migrations: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("store: no migrations were embedded")
	}
	sort.Strings(names)
	return names, nil
}
