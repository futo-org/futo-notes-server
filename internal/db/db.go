// Package db owns database engine selection and the SQL dialect seam.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	// Open selects these drivers by name: "pgx" for Postgres, "sqlite" for SQLite.
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Engine names a supported database engine.
type Engine string

// The supported database engines.
const (
	Postgres Engine = "postgres"
	SQLite   Engine = "sqlite"
)

// DefaultSQLiteURL is the database the server uses when DATABASE_URL is unset.
const DefaultSQLiteURL = "sqlite:./data/notes.db"

var placeholder = regexp.MustCompile(`\$([1-9][0-9]*)`)

// Dialect contains the deliberately small set of SQL differences between the
// two supported engines. Shared query text stays in the owning packages.
type Dialect struct {
	engine Engine
	path   string
}

// ParseDialect selects the dialect for a postgres://, postgresql://, or
// sqlite: database URL.
func ParseDialect(databaseURL string) (Dialect, error) {
	switch {
	case strings.HasPrefix(databaseURL, "postgres://"), strings.HasPrefix(databaseURL, "postgresql://"):
		return Dialect{engine: Postgres}, nil
	case strings.HasPrefix(databaseURL, "sqlite:"):
		path := strings.TrimPrefix(databaseURL, "sqlite:")
		if path == "" {
			return Dialect{}, errors.New("sqlite DATABASE_URL must name a database file")
		}
		return Dialect{engine: SQLite, path: path}, nil
	default:
		return Dialect{}, errors.New("unsupported DATABASE_URL scheme (want postgres://, postgresql://, or sqlite:)")
	}
}

// Engine returns the database engine the dialect targets.
func (d Dialect) Engine() Engine { return d.engine }

// Path returns the SQLite database file path, or "" for Postgres.
func (d Dialect) Path() string { return d.path }

// Rebind rewrites Postgres $n placeholders into the engine's form. SQLite
// gets ?n, which keeps the argument numbering.
func (d Dialect) Rebind(query string) string {
	if d.engine == SQLite {
		return placeholder.ReplaceAllString(query, `?$1`)
	}
	return query
}

// ForUpdate returns the row-locking clause to append to a SELECT, which is
// empty on SQLite where a write transaction already holds the database lock.
func (d Dialect) ForUpdate() string {
	if d.engine == Postgres {
		return " FOR UPDATE"
	}
	return ""
}

// ForUpdateSkipLocked is ForUpdate, skipping rows another transaction holds.
func (d Dialect) ForUpdateSkipLocked() string {
	if d.engine == Postgres {
		return " FOR UPDATE SKIP LOCKED"
	}
	return ""
}

// JSONCast returns the cast that makes a text parameter a JSON value.
func (d Dialect) JSONCast() string {
	if d.engine == Postgres {
		return "::jsonb"
	}
	return ""
}

// LockMutation serializes transactions that use the same key until tx ends.
// SQLite needs no lock because it admits one writer at a time.
func (d Dialect) LockMutation(ctx context.Context, tx *Tx, key string) error {
	if d.engine == SQLite {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// DB owns a sql.DB and applies placeholder rebinding at every shared SQL call.
type DB struct {
	db      *sql.DB
	dialect Dialect
}

// Wrap pairs an already opened sql.DB with the dialect its queries use.
func Wrap(database *sql.DB, dialect Dialect) *DB {
	return &DB{db: database, dialect: dialect}
}

// Dialect returns the SQL dialect of the database engine.
func (d *DB) Dialect() Dialect { return d.dialect }

// ExecContext rebinds query for the engine and executes it.
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, d.dialect.Rebind(query), args...)
}

// QueryContext rebinds query for the engine and runs it, returning its rows.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, d.dialect.Rebind(query), args...)
}

// QueryRowContext rebinds query for the engine and runs it, returning at most
// one row.
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, d.dialect.Rebind(query), args...)
}

// BeginTx starts a transaction whose queries use the same dialect.
func (d *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := d.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, dialect: d.dialect}, nil
}

// PingContext verifies that the database is reachable.
func (d *DB) PingContext(ctx context.Context) error { return d.db.PingContext(ctx) }

// Stats returns the connection pool statistics.
func (d *DB) Stats() sql.DBStats { return d.db.Stats() }

// Close closes the underlying connection pool.
func (d *DB) Close() error { return d.db.Close() }

// Scanner is the Scan method shared by *sql.Row and *sql.Rows, so one
// function can read a row from either.
type Scanner interface {
	Scan(dest ...any) error
}

// Tx applies the same dialect to transaction queries and can defer callbacks
// until after a successful commit. SQLite sync notifications use that hook.
type Tx struct {
	tx          *sql.Tx
	dialect     Dialect
	afterCommit []func()
}

// Dialect returns the SQL dialect of the database engine.
func (t *Tx) Dialect() Dialect { return t.dialect }

// ExecContext rebinds query for the engine and executes it in the transaction.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.dialect.Rebind(query), args...)
}

// QueryContext rebinds query for the engine and runs it in the transaction.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.dialect.Rebind(query), args...)
}

// QueryRowContext rebinds query for the engine and runs it in the
// transaction, returning at most one row.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.dialect.Rebind(query), args...)
}

// AfterCommit registers callback to run once the transaction commits. It is
// dropped if the transaction rolls back.
func (t *Tx) AfterCommit(callback func()) {
	t.afterCommit = append(t.afterCommit, callback)
}

// Commit commits the transaction and then runs the AfterCommit callbacks.
func (t *Tx) Commit() error {
	if err := t.tx.Commit(); err != nil {
		return err
	}
	for _, callback := range t.afterCommit {
		callback()
	}
	return nil
}

// Rollback aborts the transaction.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Discard rolls the transaction back unless it has already been committed or
// rolled back, so it is safe to defer right after BeginTx. A rollback failure
// has no caller to report to, so it is logged.
func (t *Tx) Discard() {
	if err := t.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		log.Warn().Err(err).Msg("rolling back transaction")
	}
}

// Options holds the settings Open needs besides the database URL.
type Options struct {
	// BlobDir is checked for existing blobs before a fresh SQLite database
	// is created.
	BlobDir string
	// AllowFresh creates a fresh SQLite database even when BlobDir holds
	// blobs.
	AllowFresh bool
	// PoolMax caps the open and idle Postgres connections.
	PoolMax int
	// PoolIdleTimeout closes Postgres connections idle for longer than this.
	PoolIdleTimeout time.Duration
}

// Open opens the database named by databaseURL, a postgres://,
// postgresql://, or sqlite: URL. It does not apply migrations.
func Open(databaseURL string, opts Options) (*DB, error) {
	dialect, err := ParseDialect(databaseURL)
	if err != nil {
		return nil, err
	}

	driverName := "pgx"
	dsn := databaseURL
	if dialect.engine == SQLite {
		if err := prepareSQLiteFile(dialect.path, opts.BlobDir, opts.AllowFresh); err != nil {
			return nil, err
		}
		driverName = "sqlite"
		dsn = sqliteDSN(dialect.path)
	}

	database, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	if dialect.engine == Postgres {
		database.SetMaxOpenConns(opts.PoolMax)
		database.SetMaxIdleConns(opts.PoolMax)
		database.SetConnMaxIdleTime(opts.PoolIdleTimeout)
	} else {
		database.SetMaxOpenConns(4)
		database.SetMaxIdleConns(4)
		// WAL is stored in the database file, so it is set once here rather than
		// on every connection: switching journal_mode as a connection opened,
		// while another connection held a lock, failed with SQLITE_BUSY at once
		// (busy_timeout does not cover it), roughly 1 run in 10 of
		// TestSQLiteConcurrentMigration.
		if _, err := database.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			database.Close()
			return nil, fmt.Errorf("enabling WAL: %w", err)
		}
	}
	return Wrap(database, dialect), nil
}

func sqliteDSN(path string) string {
	// Encode pragma values so a path containing '?' or '#' cannot alter the
	// connection settings. modernc applies each _pragma on every connection;
	// journal_mode is set once in Open.
	u := &url.URL{Scheme: "file", Path: path}
	if !filepath.IsAbs(path) {
		u.Opaque = path
		u.Path = ""
	}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(10000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "synchronous(NORMAL)")
	query.Set("_txlock", "immediate")
	u.RawQuery = query.Encode()
	return u.String()
}

func prepareSQLiteFile(path, blobDir string, allowFresh bool) error {
	info, statErr := os.Stat(path)
	fresh := errors.Is(statErr, os.ErrNotExist) || statErr == nil && info.Size() == 0
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("checking SQLite database %q: %w", path, statErr)
	}
	if fresh && !allowFresh {
		hasBlobs, err := containsBlobFiles(blobDir)
		if err != nil {
			return fmt.Errorf("checking BLOB_DIR before creating SQLite database: %w", err)
		}
		if hasBlobs {
			return fmt.Errorf("refusing to create fresh SQLite database %q because BLOB_DIR %q contains blob files; DATABASE_URL may have been lost or changed (set ALLOW_FRESH_DATABASE=true to override)", path, blobDir)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating SQLite database directory: %w", err)
	}
	return nil
}

func containsBlobFiles(root string) (bool, error) {
	found := false
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}

// TimestampFormat is the fixed-width ISO-8601 UTC millisecond form timestamps
// are stored in as text. Kysely records migration timestamps in the same form.
const TimestampFormat = "2006-01-02T15:04:05.000Z"

// Timestamp formats value in UTC using TimestampFormat.
func Timestamp(value time.Time) string { return value.UTC().Format(TimestampFormat) }

// Time scans both pgx time.Time values and SQLite's fixed-width TEXT form.
type Time struct{ time.Time }

// Scan implements sql.Scanner.
func (t *Time) Scan(src any) error {
	switch value := src.(type) {
	case time.Time:
		t.Time = value.UTC()
		return nil
	case string:
		parsed, err := time.Parse(TimestampFormat, value)
		if err != nil {
			return err
		}
		t.Time = parsed
		return nil
	case []byte:
		return t.Scan(string(value))
	default:
		return fmt.Errorf("cannot scan %T as timestamp", src)
	}
}
