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

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"futo-notes-server/internal/config"
)

type Engine string

const (
	Postgres Engine = "postgres"
	SQLite   Engine = "sqlite"
)

const DefaultSQLiteURL = "sqlite:./data/notes.db"

var placeholder = regexp.MustCompile(`\$([1-9][0-9]*)`)

// Dialect contains the deliberately small set of SQL differences between the
// two supported engines. Shared query text stays in the owning packages.
type Dialect struct {
	engine Engine
	path   string
}

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
		return Dialect{}, fmt.Errorf("unsupported DATABASE_URL scheme (want postgres://, postgresql://, or sqlite:)")
	}
}

func (d Dialect) Engine() Engine { return d.engine }
func (d Dialect) Path() string   { return d.path }

func (d Dialect) Rebind(query string) string {
	if d.engine == SQLite {
		return placeholder.ReplaceAllString(query, `?$1`)
	}
	return query
}

func (d Dialect) ForUpdate() string {
	if d.engine == Postgres {
		return " FOR UPDATE"
	}
	return ""
}

func (d Dialect) ForUpdateSkipLocked() string {
	if d.engine == Postgres {
		return " FOR UPDATE SKIP LOCKED"
	}
	return ""
}

func (d Dialect) JSONCast() string {
	if d.engine == Postgres {
		return "::jsonb"
	}
	return ""
}

func (d Dialect) LockMutation(ctx context.Context, tx *Tx, key string) error {
	if d.engine == SQLite {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// DB owns a sql.DB and applies placeholder rebinding at every shared SQL call.
type DB struct {
	*sql.DB
	dialect Dialect
}

func Wrap(database *sql.DB, dialect Dialect) *DB {
	return &DB{DB: database, dialect: dialect}
}

func (d *DB) Dialect() Dialect { return d.dialect }

func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(ctx, d.dialect.Rebind(query), args...)
}

func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	return d.DB.Exec(d.dialect.Rebind(query), args...)
}

func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, d.dialect.Rebind(query), args...)
}

func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.DB.Query(d.dialect.Rebind(query), args...)
}

func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, d.dialect.Rebind(query), args...)
}

func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.DB.QueryRow(d.dialect.Rebind(query), args...)
}

func (d *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := d.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, dialect: d.dialect}, nil
}

// Tx applies the same dialect to transaction queries and can defer callbacks
// until after a successful commit. SQLite sync notifications use that hook.
type Tx struct {
	*sql.Tx
	dialect     Dialect
	afterCommit []func()
}

func WrapTx(tx *sql.Tx, dialect Dialect) *Tx {
	return &Tx{Tx: tx, dialect: dialect}
}

func (t *Tx) Dialect() Dialect { return t.dialect }

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(t.dialect.Rebind(query), args...)
}

func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.Tx.Query(t.dialect.Rebind(query), args...)
}

func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.Tx.QueryRow(t.dialect.Rebind(query), args...)
}

func (t *Tx) AfterCommit(callback func()) {
	t.afterCommit = append(t.afterCommit, callback)
}

func (t *Tx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	for _, callback := range t.afterCommit {
		callback()
	}
	return nil
}

func Open(cfg config.Config) (*DB, error) {
	dialect, err := ParseDialect(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	driverName := "pgx"
	dsn := cfg.DatabaseURL
	if dialect.engine == SQLite {
		if err := prepareSQLiteFile(dialect.path, cfg.BlobDir, cfg.AllowFreshDatabase); err != nil {
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
		database.SetMaxOpenConns(cfg.DBPoolMax)
		database.SetMaxIdleConns(cfg.DBPoolMax)
		database.SetConnMaxIdleTime(cfg.DBPoolIdleTimeout)
	} else {
		database.SetMaxOpenConns(4)
		database.SetMaxIdleConns(4)
	}
	return Wrap(database, dialect), nil
}

func sqliteDSN(path string) string {
	// Encode pragma values so a path containing '?' or '#' cannot alter the
	// connection settings. modernc applies each _pragma on every connection.
	u := &url.URL{Scheme: "file", Path: path}
	if !filepath.IsAbs(path) {
		u.Opaque = path
		u.Path = ""
	}
	query := u.Query()
	query.Add("_pragma", "journal_mode(WAL)")
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

const TimestampFormat = "2006-01-02T15:04:05.000Z"

func Timestamp(value time.Time) string { return value.UTC().Format(TimestampFormat) }

// Time scans both pgx time.Time values and SQLite's fixed-width TEXT form.
type Time struct{ time.Time }

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

type NullTime struct {
	Time  time.Time
	Valid bool
}

func (t *NullTime) Scan(src any) error {
	if src == nil {
		t.Time = time.Time{}
		t.Valid = false
		return nil
	}
	var value Time
	if err := value.Scan(src); err != nil {
		return err
	}
	t.Time = value.Time
	t.Valid = true
	return nil
}
