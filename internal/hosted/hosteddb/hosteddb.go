// Package hosteddb is the hosted service's migration track: schema the hosted
// API needs on top of the shared Postgres schema (internal/db), which
// self-hosted databases never see. It keeps its own bookkeeping table, so the
// shared track's history stays exactly what the self-hosted server and the
// TypeScript server expect.
package hosteddb

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"path"
	"strings"

	"futo-notes-server/internal/db"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const bookkeepingTable = "hosted_migrations"

// lockKey serializes migration runs, distinct from the shared track's key.
const lockKey int64 = 7376088471349217

type migration struct {
	name string
	sql  string
}

// Migrate applies the hosted migrations the database lacks and returns their
// names. Run it after db.Migrate: hosted migrations alter shared tables.
func Migrate(ctx context.Context, database *db.DB) ([]string, error) {
	if database.Dialect().Engine() != db.Postgres {
		return nil, errors.New("the hosted schema needs Postgres")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Discard()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("acquiring hosted migration lock: %w", err)
	}
	pending, err := pendingIn(ctx, tx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pending))
	for _, m := range pending {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return nil, fmt.Errorf("applying hosted migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+bookkeepingTable+` (name) VALUES ($1)`, m.name); err != nil {
			return nil, fmt.Errorf("recording hosted migration %s: %w", m.name, err)
		}
		names = append(names, m.name)
	}
	return names, tx.Commit()
}

// Pending returns the names of the hosted migrations Migrate would apply,
// without applying them. It fails on a history Migrate would refuse.
func Pending(ctx context.Context, database *db.DB) ([]string, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Rolled back: checking a fresh database leaves no bookkeeping behind.
	defer tx.Discard()
	pending, err := pendingIn(ctx, tx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pending))
	for _, m := range pending {
		names = append(names, m.name)
	}
	return names, nil
}

// pendingIn returns the migrations tx's database has not recorded, creating
// the bookkeeping table if needed. It refuses a recorded name this binary does
// not ship, and a recorded migration that follows an unrecorded one.
func pendingIn(ctx context.Context, tx *db.Tx) ([]migration, error) {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+bookkeepingTable+` (
		name text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("creating hosted migration bookkeeping: %w", err)
	}
	all, err := load()
	if err != nil {
		return nil, fmt.Errorf("loading hosted migrations: %w", err)
	}
	applied, err := appliedIn(ctx, tx)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, m := range all {
		known[m.name] = true
	}
	for name := range applied {
		if !known[name] {
			return nil, fmt.Errorf("database records hosted migration %q, which this binary does not ship; it was migrated by a newer API", name)
		}
	}
	var pending []migration
	for _, m := range all {
		if applied[m.name] {
			if len(pending) > 0 {
				return nil, fmt.Errorf("hosted migration history has a gap: %q is recorded but %q is not", m.name, pending[0].name)
			}
			continue
		}
		pending = append(pending, m)
	}
	return pending, nil
}

func appliedIn(ctx context.Context, tx *db.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM `+bookkeepingTable)
	if err != nil {
		return nil, fmt.Errorf("reading applied hosted migrations: %w", err)
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

// load returns the embedded migrations in order. ReadDir sorts by filename,
// and filenames are zero-padded.
func load() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		body, err := migrationFS.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, migration{name: strings.TrimSuffix(entry.Name(), ".sql"), sql: string(body)})
	}
	return migrations, nil
}
