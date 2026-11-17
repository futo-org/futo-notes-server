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

	"futo-notes-server/internal/db"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const bookkeepingTable = "hosted_migrations"

var track = db.MigrationTrack{
	Label: "hosted migration",
	Files: migrationFS,
	Dir:   "migrations",
	// Distinct from the shared track's key.
	LockKey: 7376088471349217,
	Bookkeeping: []string{`CREATE TABLE IF NOT EXISTS ` + bookkeepingTable + ` (
		name text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`},
	Table:  bookkeepingTable,
	Record: `INSERT INTO ` + bookkeepingTable + ` (name, applied_at) VALUES ($1, $2)`,
}

// Migrate applies the hosted migrations the database lacks and returns their
// names. Run it after db.Migrate: hosted migrations alter shared tables.
func Migrate(ctx context.Context, database *db.DB) ([]string, error) {
	if database.Dialect().Engine() != db.Postgres {
		return nil, errors.New("the hosted schema needs Postgres")
	}
	return db.MigrateTrack(ctx, database, track)
}

// Pending returns the names of the hosted migrations Migrate would apply,
// without applying them. It fails on a history Migrate would refuse.
func Pending(ctx context.Context, database *db.DB) ([]string, error) {
	return db.PendingTrack(ctx, database, track)
}
