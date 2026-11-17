package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
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

// MigrationTrack is one migration history kept in a database: the migrations
// a binary ships, and the bookkeeping that records which of them ran.
type MigrationTrack struct {
	// Label names the track in errors, such as "migration".
	Label string
	// Files holds the track's migrations as Dir/*.sql. They run in filename
	// order, and a migration's name is its filename without ".sql".
	Files fs.FS
	Dir   string
	// LockKey is the Postgres advisory lock that serializes runs.
	LockKey int64
	// Bookkeeping creates the bookkeeping table Table, if missing. Table has a
	// name column listing the migrations applied.
	Bookkeeping []string
	Table       string
	// Record inserts an applied migration: $1 is its name and $2 the time it
	// ran, formatted with TimestampFormat.
	Record string
}

func sharedTrack(dialect Dialect) MigrationTrack {
	return MigrationTrack{
		Label:   "migration",
		Files:   migrationFS,
		Dir:     path.Join("migrations", string(dialect.Engine())),
		LockKey: advisoryLockKey,
		Bookkeeping: []string{
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
		},
		Table:  migrationTable,
		Record: `insert into ` + migrationTable + ` (name, "timestamp") values ($1, $2)`,
	}
}

type migration struct {
	name string
	sql  string
}

func loadMigrations(dialect Dialect) ([]migration, error) {
	return sharedTrack(dialect).load()
}

func (t MigrationTrack) load() ([]migration, error) {
	// ReadDir returns entries sorted by filename, and filenames are
	// zero-padded, so this is migration order.
	entries, err := fs.ReadDir(t.Files, t.Dir)
	if err != nil {
		return nil, err
	}

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		body, err := fs.ReadFile(t.Files, path.Join(t.Dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, migration{
			name: strings.TrimSuffix(entry.Name(), ".sql"),
			sql:  string(body),
		})
	}
	return migrations, nil
}

// Migrate brings the database up to the latest migration and returns the names
// of the migrations it applied. It applies nothing against a database that
// already has every migration recorded, which is the case when swapping in for
// the TypeScript server.
func Migrate(ctx context.Context, database *DB) ([]string, error) {
	return MigrateTrack(ctx, database, sharedTrack(database.Dialect()))
}

// Pending returns the names of the migrations Migrate would apply, without
// applying them. It fails on a history Migrate would refuse.
func Pending(ctx context.Context, database *DB) ([]string, error) {
	return PendingTrack(ctx, database, sharedTrack(database.Dialect()))
}

// MigrateTrack applies the migrations of track that the database has not
// recorded, in one transaction, and returns their names.
func MigrateTrack(ctx context.Context, database *DB, track MigrationTrack) ([]string, error) {
	migrations, err := track.load()
	if err != nil {
		return nil, fmt.Errorf("loading %ss: %w", track.Label, err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Discard()

	if database.Dialect().Engine() == Postgres {
		if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock($1)`, track.LockKey); err != nil {
			return nil, fmt.Errorf("acquiring %s lock: %w", track.Label, err)
		}
	}

	pending, err := track.pending(ctx, tx, migrations)
	if err != nil {
		return nil, err
	}

	// Distinct, increasing timestamps so that ordering by them reproduces
	// the order the migrations actually ran in.
	stamp := time.Now().UTC()
	names := make([]string, 0, len(pending))
	for i, m := range pending {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return nil, fmt.Errorf("applying %s %s: %w", track.Label, m.name, err)
		}
		if _, err := tx.ExecContext(ctx, track.Record,
			m.name, stamp.Add(time.Duration(i)*time.Millisecond).Format(TimestampFormat),
		); err != nil {
			return nil, fmt.Errorf("recording %s %s: %w", track.Label, m.name, err)
		}
		names = append(names, m.name)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return names, nil
}

// PendingTrack returns the names of the migrations MigrateTrack would apply,
// without applying them. It fails on a history MigrateTrack would refuse.
func PendingTrack(ctx context.Context, database *DB, track MigrationTrack) ([]string, error) {
	migrations, err := track.load()
	if err != nil {
		return nil, fmt.Errorf("loading %ss: %w", track.Label, err)
	}
	// Rolled back: a database that has never been migrated reports every
	// migration without gaining bookkeeping tables.
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Discard()
	pending, err := track.pending(ctx, tx, migrations)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pending))
	for _, m := range pending {
		names = append(names, m.name)
	}
	return names, nil
}

// pending creates the bookkeeping if needed and returns the migrations still
// to run. It refuses to proceed on a history this binary cannot account for: a
// name recorded that we do not ship (the database is newer, or belongs to
// another server), or a gap where an earlier migration is missing while a
// later one is recorded.
func (t MigrationTrack) pending(ctx context.Context, tx *Tx, migrations []migration) ([]migration, error) {
	for _, stmt := range t.Bookkeeping {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("creating %s bookkeeping: %w", t.Label, err)
		}
	}
	applied, err := t.applied(ctx, tx)
	if err != nil {
		return nil, err
	}

	known := map[string]bool{}
	for _, m := range migrations {
		known[m.name] = true
	}
	for name := range applied {
		if !known[name] {
			return nil, fmt.Errorf(
				"database records %s %q, which this server does not ship; it was migrated by a newer server",
				t.Label, name,
			)
		}
	}

	var pending []migration
	for _, m := range migrations {
		if applied[m.name] {
			if len(pending) > 0 {
				return nil, fmt.Errorf(
					"%s history has a gap: %q is recorded but %q is not",
					t.Label, m.name, pending[0].name,
				)
			}
			continue
		}
		pending = append(pending, m)
	}
	return pending, nil
}

func (t MigrationTrack) applied(ctx context.Context, tx *Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `select name from `+t.Table)
	if err != nil {
		return nil, fmt.Errorf("reading applied %ss: %w", t.Label, err)
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
