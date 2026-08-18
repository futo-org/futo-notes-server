package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// The bookkeeping tables keep their Kysely names and shapes so that a database
// written by the TypeScript server is recognized as already migrated, and a
// database written here is recognized by the TypeScript server in turn.
const (
	migrationTable = "kysely_migration"
	lockTable      = "kysely_migration_lock"
	lockRowID      = "migration_lock"
)

// advisoryLockKey guards the migration run itself, so two servers booting
// against the same fresh database do not both try to create the schema.
const advisoryLockKey int64 = 7376088471349216

// kyselyTimestampFormat is the ISO-8601 millisecond form Kysely records.
const kyselyTimestampFormat = "2006-01-02T15:04:05.000Z"

type migration struct {
	name string
	sql  string
}

func loadMigrations() ([]migration, error) {
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
		migrations = append(migrations, migration{
			name: strings.TrimSuffix(entry.Name(), ".sql"),
			sql:  string(body),
		})
	}

	// Filenames are zero-padded, so lexical order is migration order.
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].name < migrations[j].name })
	return migrations, nil
}

// Migrate brings the database up to the latest migration and returns the names
// of the migrations it applied. It applies nothing against a database that
// already has every migration recorded, which is the case when swapping in for
// the TypeScript server.
func Migrate(ctx context.Context, database *sql.DB) ([]string, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, fmt.Errorf("loading migrations: %w", err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("acquiring migration lock: %w", err)
	}

	if err := ensureBookkeeping(ctx, tx); err != nil {
		return nil, err
	}

	applied, err := appliedMigrations(ctx, tx)
	if err != nil {
		return nil, err
	}

	pending, err := pendingMigrations(migrations, applied)
	if err != nil {
		return nil, err
	}

	// Distinct, increasing timestamps so that ordering by them reproduces
	// the order the migrations actually ran in.
	stamp := time.Now().UTC()
	names := make([]string, 0, len(pending))
	for i, m := range pending {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return nil, fmt.Errorf("applying migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`insert into `+migrationTable+` (name, "timestamp") values ($1, $2)`,
			m.name, stamp.Add(time.Duration(i)*time.Millisecond).Format(kyselyTimestampFormat),
		); err != nil {
			return nil, fmt.Errorf("recording migration %s: %w", m.name, err)
		}
		names = append(names, m.name)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return names, nil
}

func ensureBookkeeping(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`create table if not exists ` + migrationTable + ` (
			name varchar(255) primary key,
			"timestamp" varchar(255) not null
		)`,
		`create table if not exists ` + lockTable + ` (
			id varchar(255) primary key,
			is_locked integer not null default 0
		)`,
		`insert into ` + lockTable + ` (id, is_locked) values ('` + lockRowID + `', 0)
			on conflict (id) do nothing`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("creating migration bookkeeping: %w", err)
		}
	}
	return nil
}

func appliedMigrations(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `select name from `+migrationTable)
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
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

// pendingMigrations returns the migrations still to run, and refuses to
// proceed on a history this binary cannot account for: a name recorded that we
// do not ship (the database is newer, or belongs to another server), or a gap
// where an earlier migration is missing while a later one is recorded.
func pendingMigrations(migrations []migration, applied map[string]bool) ([]migration, error) {
	known := map[string]bool{}
	for _, m := range migrations {
		known[m.name] = true
	}
	for name := range applied {
		if !known[name] {
			return nil, fmt.Errorf(
				"database records migration %q, which this server does not ship; it was migrated by a newer server",
				name,
			)
		}
	}

	var pending []migration
	for _, m := range migrations {
		if applied[m.name] {
			if len(pending) > 0 {
				return nil, fmt.Errorf(
					"migration history has a gap: %q is recorded but %q is not",
					m.name, pending[0].name,
				)
			}
			continue
		}
		pending = append(pending, m)
	}
	return pending, nil
}
