package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"futo-notes-server/internal/config"
)

type copyTable struct {
	name        string
	columns     []string
	jsonColumns map[string]bool
}

var sqliteCopyTables = []copyTable{
	{name: "users", columns: []string{"id", "sub", "name", "email"}},
	{name: "collections", columns: []string{"id", "user_id", "created_at", "current_version", "key_salt", "key_kdf", "encrypted_vault_key", "key_updated_at"}, jsonColumns: map[string]bool{"key_kdf": true}},
	{name: "objects", columns: []string{"id", "collection_id", "user_id", "version", "deleted", "blob_key", "size_bytes", "created_at", "updated_at", "change_seq"}},
	{name: "blob_ledger", columns: []string{"blob_key", "user_id", "size_bytes", "state", "collection_id", "object_id", "object_version", "created_at", "state_changed_at"}},
	{name: "mutation_results", columns: []string{"user_id", "mutation_id", "kind", "collection_id", "object_id", "requested_version", "result", "created_at"}, jsonColumns: map[string]bool{"result": true}},
	{name: "sessions", columns: []string{"id", "user_id", "access_token_hash", "expires_at"}},
	{name: "server_config", columns: []string{"key", "value"}},
}

// MigratePostgresToSQLite copies a consistent Postgres snapshot into a new
// SQLite database, verifies it, and leaves the source untouched.
func MigratePostgresToSQLite(ctx context.Context, sourceURL, targetURL string, output io.Writer) (err error) {
	sourceDialect, err := ParseDialect(sourceURL)
	if err != nil {
		return fmt.Errorf("source database: %w", err)
	}
	if sourceDialect.Engine() != Postgres {
		return errors.New("migrate-to-sqlite requires DATABASE_URL to be postgres:// or postgresql://")
	}
	targetDialect, err := ParseDialect(targetURL)
	if err != nil {
		return fmt.Errorf("target database: %w", err)
	}
	if targetDialect.Engine() != SQLite {
		return errors.New("-to must be a sqlite: URL")
	}
	if err := refuseNonemptyTarget(targetDialect.Path()); err != nil {
		return err
	}
	// A zero-byte file is not a database and is safe to replace.
	if err := os.Remove(targetDialect.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing empty SQLite target %q: %w", targetDialect.Path(), err)
	}

	completed := false
	defer func() {
		if !completed {
			if cleanupErr := removeSQLiteFiles(targetDialect.Path()); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("removing partial SQLite target: %w", cleanupErr))
			}
		}
	}()

	source, err := sql.Open("pgx", sourceURL)
	if err != nil {
		return fmt.Errorf("opening Postgres source: %w", err)
	}
	defer source.Close()
	if err := source.PingContext(ctx); err != nil {
		return fmt.Errorf("connecting to Postgres source: %w", err)
	}
	snapshot, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("starting Postgres snapshot: %w", err)
	}
	defer snapshot.Rollback()

	target, err := Open(config.Config{
		DatabaseURL:        targetURL,
		BlobDir:            "",
		AllowFreshDatabase: true,
	})
	if err != nil {
		return fmt.Errorf("opening SQLite target: %w", err)
	}
	defer target.Close()
	if _, err := Migrate(ctx, target); err != nil {
		return fmt.Errorf("creating SQLite schema: %w", err)
	}
	targetTx, err := target.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting SQLite copy: %w", err)
	}
	defer targetTx.Rollback()

	counts := make(map[string]int64, len(sqliteCopyTables))
	for _, table := range sqliteCopyTables {
		count, err := copyPostgresTable(ctx, snapshot, targetTx, table)
		if err != nil {
			return fmt.Errorf("copying %s: %w", table.name, err)
		}
		counts[table.name] = count
	}
	if err := verifySnapshot(ctx, snapshot, targetTx, counts); err != nil {
		return err
	}
	if err := targetTx.Commit(); err != nil {
		return fmt.Errorf("committing SQLite copy: %w", err)
	}
	if err := verifySQLite(ctx, target); err != nil {
		return err
	}

	writeMigrationSummary(output, counts)
	completed = true
	return nil
}

func refuseNonemptyTarget(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("checking SQLite target %q: %w", path, err)
	case info.IsDir():
		return fmt.Errorf("SQLite target %q is a directory", path)
	case info.Size() != 0:
		return fmt.Errorf("refusing existing non-empty SQLite target %q", path)
	default:
		return nil
	}
}

func removeSQLiteFiles(path string) error {
	var cleanupErrors []error
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("%s: %w", candidate, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func copyPostgresTable(ctx context.Context, source *sql.Tx, target *Tx, table copyTable) (int64, error) {
	columns := strings.Join(table.columns, ", ")
	rows, err := source.QueryContext(ctx, `SELECT `+columns+` FROM `+table.name)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	parameters := make([]string, len(table.columns))
	for i := range parameters {
		parameters[i] = fmt.Sprintf("$%d", i+1)
	}
	insert := `INSERT INTO ` + table.name + ` (` + columns + `) VALUES (` + strings.Join(parameters, ", ") + `)`

	var count int64
	for rows.Next() {
		values := make([]any, len(table.columns))
		destinations := make([]any, len(values))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return count, err
		}
		for i, column := range table.columns {
			values[i], err = sqliteCopyValue(values[i], table.jsonColumns[column])
			if err != nil {
				return count, fmt.Errorf("column %s: %w", column, err)
			}
		}
		if _, err := target.ExecContext(ctx, insert, values...); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

func sqliteCopyValue(value any, compactJSON bool) (any, error) {
	if value == nil {
		return nil, nil
	}
	if timestamp, ok := value.(time.Time); ok {
		return Timestamp(timestamp), nil
	}
	if !compactJSON {
		return value, nil
	}
	var source []byte
	switch value := value.(type) {
	case []byte:
		source = value
	case string:
		source = []byte(value)
	default:
		return nil, fmt.Errorf("unexpected JSON value %T", value)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, source); err != nil {
		return nil, err
	}
	return compact.String(), nil
}

func verifySnapshot(ctx context.Context, source *sql.Tx, target *Tx, copied map[string]int64) error {
	for _, table := range sqliteCopyTables {
		var sourceCount, targetCount int64
		if err := source.QueryRowContext(ctx, `SELECT count(*) FROM `+table.name).Scan(&sourceCount); err != nil {
			return fmt.Errorf("verifying Postgres %s count: %w", table.name, err)
		}
		if err := target.QueryRowContext(ctx, `SELECT count(*) FROM `+table.name).Scan(&targetCount); err != nil {
			return fmt.Errorf("verifying SQLite %s count: %w", table.name, err)
		}
		if sourceCount != copied[table.name] || targetCount != sourceCount {
			return fmt.Errorf("%s row-count mismatch: source=%d copied=%d target=%d", table.name, sourceCount, copied[table.name], targetCount)
		}
	}

	sourceVersions, err := collectionVersions(ctx, source)
	if err != nil {
		return fmt.Errorf("verifying Postgres collection versions: %w", err)
	}
	targetVersions, err := collectionVersions(ctx, target)
	if err != nil {
		return fmt.Errorf("verifying SQLite collection versions: %w", err)
	}
	if len(sourceVersions) != len(targetVersions) {
		return fmt.Errorf("collection-version verification count mismatch: source=%d target=%d", len(sourceVersions), len(targetVersions))
	}
	for id, sourceVersion := range sourceVersions {
		targetVersion, ok := targetVersions[id]
		if !ok || targetVersion != sourceVersion {
			return fmt.Errorf("collection %s version mismatch: source=%d target=%d", id, sourceVersion, targetVersion)
		}
	}

	var sourceBytes, targetBytes int64
	if err := source.QueryRowContext(ctx, `SELECT coalesce(sum(size_bytes), 0) FROM blob_ledger`).Scan(&sourceBytes); err != nil {
		return fmt.Errorf("verifying Postgres blob ledger bytes: %w", err)
	}
	if err := target.QueryRowContext(ctx, `SELECT coalesce(sum(size_bytes), 0) FROM blob_ledger`).Scan(&targetBytes); err != nil {
		return fmt.Errorf("verifying SQLite blob ledger bytes: %w", err)
	}
	if sourceBytes != targetBytes {
		return fmt.Errorf("blob ledger byte mismatch: source=%d target=%d", sourceBytes, targetBytes)
	}
	return nil
}

type rowQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func collectionVersions(ctx context.Context, queryer rowQueryer) (map[string]int64, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT c.id, c.current_version, coalesce(max(o.change_seq), 0)
		FROM collections c LEFT JOIN objects o ON o.collection_id = c.id
		GROUP BY c.id, c.current_version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := make(map[string]int64)
	for rows.Next() {
		var id string
		var current, maximum int64
		if err := rows.Scan(&id, &current, &maximum); err != nil {
			return nil, err
		}
		if maximum != current {
			return nil, fmt.Errorf("collection %s has max(change_seq) %d but current_version %d", id, maximum, current)
		}
		versions[id] = current
	}
	return versions, rows.Err()
}

func verifySQLite(ctx context.Context, target *DB) error {
	var integrity string
	if err := target.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("running SQLite integrity_check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("SQLite integrity_check: %s", integrity)
	}
	rows, err := target.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("running SQLite foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID int64
		var parent string
		var constraint int
		if err := rows.Scan(&table, &rowID, &parent, &constraint); err != nil {
			return fmt.Errorf("reading SQLite foreign_key_check: %w", err)
		}
		return fmt.Errorf("SQLite foreign_key_check failed: table=%s rowid=%d parent=%s constraint=%d", table, rowID, parent, constraint)
	}
	return rows.Err()
}

func writeMigrationSummary(output io.Writer, counts map[string]int64) {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "TABLE\tROWS")
	for _, table := range sqliteCopyTables {
		fmt.Fprintf(writer, "%s\t%d\n", table.name, counts[table.name])
	}
	fmt.Fprintln(writer, "checks\trow counts, collection versions, blob bytes, integrity, foreign keys: ok")
	_ = writer.Flush()
}
