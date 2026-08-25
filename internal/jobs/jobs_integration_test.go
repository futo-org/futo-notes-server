package jobs_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/blobs"
	databasepkg "futo-notes-server/internal/db"
	"futo-notes-server/internal/jobs"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestRecurringJobs(t *testing.T) {
	databaseURL := os.Getenv("JOBS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBS_TEST_DATABASE_URL is not set")
	}
	rawDatabase, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	dialect, err := databasepkg.ParseDialect(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	database := databasepkg.Wrap(rawDatabase, dialect)
	defer database.Close()
	ctx := context.Background()
	if _, err := databasepkg.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}

	userID := uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'jobs test', $3)`, userID, "jobs-test-"+userID, userID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)

	t.Run("session reaper", func(t *testing.T) {
		if _, err := jobs.ReapSessions(ctx, database); err != nil {
			t.Fatal(err)
		}
		expiredID, liveID := uuid.NewV7().String(), uuid.NewV7().String()
		if _, err := database.ExecContext(ctx, `INSERT INTO sessions
			(id, user_id, access_token_hash, expires_at) VALUES
			($1, $3, $4, now() - interval '1 second'),
			($2, $3, $5, now() + interval '1 hour')`, expiredID, liveID, userID,
			[]byte("jobs-expired-"+expiredID), []byte("jobs-live-"+liveID)); err != nil {
			t.Fatal(err)
		}

		result, err := jobs.ReapSessions(ctx, database)
		if err != nil {
			t.Fatal(err)
		}
		if result.Reaped != 1 {
			t.Fatalf("reaped = %d, want 1", result.Reaped)
		}
		assertRowCount(t, database, `SELECT count(*) FROM sessions WHERE id = $1`, 0, expiredID)
		assertRowCount(t, database, `SELECT count(*) FROM sessions WHERE id = $1`, 1, liveID)
	})

	t.Run("storage reconciliation", func(t *testing.T) {
		store := &blobs.Store{Dir: t.TempDir()}
		orphanKey := userID + "/" + uuid.NewV7().String()
		if err := store.Put(orphanKey, []byte("orphan")); err != nil {
			t.Fatal(err)
		}
		bogusOwner := uuid.NewV7().String()
		bogusKey := bogusOwner + "/" + uuid.NewV7().String()
		if err := store.Put(bogusKey, []byte("junk")); err != nil {
			t.Fatal(err)
		}
		existingKey := userID + "/" + uuid.NewV7().String()
		if err := store.Put(existingKey, []byte("existing file")); err != nil {
			t.Fatal(err)
		}
		if _, err := database.ExecContext(ctx, `INSERT INTO blob_ledger
			(blob_key, user_id, size_bytes, state) VALUES ($1, $2, 999, 'claimed')`, existingKey, userID); err != nil {
			t.Fatal(err)
		}

		result, err := jobs.ReconcileStorage(ctx, database, store)
		if err != nil {
			t.Fatal(err)
		}
		if result.Adopted != 1 || result.Skipped != 1 || result.CapHit {
			t.Fatalf("unexpected result: %#v", result)
		}
		var state string
		var size int64
		if err := database.QueryRowContext(ctx, `SELECT state, size_bytes FROM blob_ledger WHERE blob_key = $1`, orphanKey).
			Scan(&state, &size); err != nil {
			t.Fatal(err)
		}
		if state != "staged" || size != 6 {
			t.Fatalf("adopted row = state %q size %d", state, size)
		}
		assertRowCount(t, database, `SELECT count(*) FROM blob_ledger WHERE blob_key = $1`, 0, bogusKey)
		if err := database.QueryRowContext(ctx, `SELECT state, size_bytes FROM blob_ledger WHERE blob_key = $1`, existingKey).
			Scan(&state, &size); err != nil {
			t.Fatal(err)
		}
		if state != "claimed" || size != 999 {
			t.Fatalf("existing row was changed: state %q size %d", state, size)
		}
	})

	t.Run("storage reconciliation cap", func(t *testing.T) {
		store := &blobs.Store{Dir: t.TempDir()}
		for i := range 501 {
			key := userID + "/cap-" + fmt.Sprintf("%03d", i)
			if err := store.Put(key, []byte{byte(i)}); err != nil {
				t.Fatal(err)
			}
		}
		result, err := jobs.ReconcileStorage(ctx, database, store)
		if err != nil {
			t.Fatal(err)
		}
		if result.Adopted != 500 || !result.CapHit {
			t.Fatalf("unexpected cap result: %#v", result)
		}
		assertRowCount(t, database, `SELECT count(*) FROM blob_ledger
			WHERE user_id = $1 AND blob_key LIKE $2`, 500, userID, userID+"/cap-%")
		result, err = jobs.ReconcileStorage(ctx, database, store)
		if err != nil {
			t.Fatal(err)
		}
		if result.Adopted != 1 || result.CapHit {
			t.Fatalf("unexpected follow-up result: %#v", result)
		}
		assertRowCount(t, database, `SELECT count(*) FROM blob_ledger
			WHERE user_id = $1 AND blob_key LIKE $2`, 501, userID, userID+"/cap-%")
	})

	t.Run("mutation result expiry", func(t *testing.T) {
		if _, err := jobs.ExpireMutationResults(ctx, database); err != nil {
			t.Fatal(err)
		}
		collectionID := uuid.NewV7().String()
		old := time.Now().Add(-31 * 24 * time.Hour)
		pendingOld := time.Now().Add(-25 * time.Hour)
		fresh := time.Now()
		rows := []struct {
			id, kind, result string
			created          time.Time
		}{
			{"old-pending-wrapped", "update", `{"status":102,"body":{"error":"mutation still in progress"}}`, pendingOld},
			{"old-pending-legacy", "delete", `{"status":"pending"}`, pendingOld},
			{"fresh-pending-wrapped", "update", `{"status":102,"body":{}}`, fresh},
			{"fresh-pending-legacy", "delete", `{"status":"pending"}`, fresh},
			{"successful-create-wrapped", "create", `{"status":201,"body":{"object":{"id":"durable"}}}`, old},
			{"successful-create-legacy", "create", `{"object":{"id":"legacy"}}`, old},
			{"ambiguous-create-legacy", "create", `{"error":"unknown legacy result"}`, old},
			{"failed-create-wrapped", "create", `{"status":409,"body":{"error":"version conflict"}}`, old},
			{"failed-create-legacy", "create", `{"error":"version conflict"}`, old},
			{"old-update-wrapped", "update", `{"status":200,"body":{}}`, old},
			{"old-update-legacy", "update", `{}`, old},
			{"fresh-delete", "delete", `{"status":200,"body":{}}`, fresh},
		}
		for _, row := range rows {
			if _, err := database.ExecContext(ctx, `INSERT INTO mutation_results
				(user_id, mutation_id, kind, collection_id, result, created_at)
				VALUES ($1, $2, $3, $4, $5::jsonb, $6)`, userID, row.id, row.kind,
				collectionID, row.result, row.created); err != nil {
				t.Fatalf("insert %s: %v", row.id, err)
			}
		}

		result, err := jobs.ExpireMutationResults(ctx, database)
		if err != nil {
			t.Fatal(err)
		}
		resultRows, err := database.QueryContext(ctx, `SELECT mutation_id FROM mutation_results WHERE user_id = $1`, userID)
		if err != nil {
			t.Fatal(err)
		}
		defer resultRows.Close()
		var remaining []string
		for resultRows.Next() {
			var id string
			if err := resultRows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			remaining = append(remaining, id)
		}
		if err := resultRows.Err(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(remaining)
		want := []string{"ambiguous-create-legacy", "fresh-delete", "fresh-pending-legacy",
			"fresh-pending-wrapped", "successful-create-legacy", "successful-create-wrapped"}
		if result.PendingExpired != 2 || result.OtherExpired != 4 {
			t.Fatalf("unexpected expiry result: %#v; remaining = %v", result, remaining)
		}
		if !slices.Equal(remaining, want) {
			t.Fatalf("remaining = %v, want %v", remaining, want)
		}
	})

	t.Run("blob garbage collection", func(t *testing.T) {
		store := &blobs.Store{Dir: t.TempDir()}
		if _, err := jobs.GarbageCollectBlobs(ctx, database, store); err != nil {
			t.Fatal(err)
		}
		oldTwoDays := time.Now().Add(-48 * time.Hour)
		oldTwoYears := time.Now().Add(-2 * 365 * 24 * time.Hour)
		fresh := time.Now()
		rows := []struct {
			name, state string
			changed     time.Time
			withFile    bool
			purged      bool
		}{
			{"expired-staged", "staged", oldTwoDays, true, true},
			{"fresh-staged", "staged", fresh, true, false},
			{"purgeable", "purgeable", fresh, true, true},
			{"old-retained", "retained", oldTwoYears, true, true},
			{"fresh-retained", "retained", fresh, true, false},
			{"claimed", "claimed", oldTwoYears, true, false},
			{"legacy-shared", "legacy_shared", oldTwoYears, true, false},
			{"missing-file", "staged", oldTwoDays, false, true},
		}
		for _, row := range rows {
			key := userID + "/gc-" + row.name
			if row.withFile {
				if err := store.Put(key, []byte(row.name)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.ExecContext(ctx, `INSERT INTO blob_ledger
				(blob_key, user_id, size_bytes, state, state_changed_at)
				VALUES ($1, $2, $3, $4, $5)`, key, userID, len(row.name), row.state, row.changed); err != nil {
				t.Fatalf("insert %s: %v", row.name, err)
			}
		}

		result, err := jobs.GarbageCollectBlobs(ctx, database, store)
		if err != nil {
			t.Fatal(err)
		}
		if result.RowsPurged != 4 || result.FilesRemoved != 3 {
			t.Fatalf("unexpected GC result: %#v", result)
		}
		for _, row := range rows {
			key := userID + "/gc-" + row.name
			wantRows := 1
			if row.purged {
				wantRows = 0
			}
			assertRowCount(t, database, `SELECT count(*) FROM blob_ledger WHERE blob_key = $1`, wantRows, key)
			_, statErr := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(key)))
			if row.purged && row.withFile && !os.IsNotExist(statErr) {
				t.Fatalf("purged file %q still exists: %v", key, statErr)
			}
			if !row.purged && row.withFile && statErr != nil {
				t.Fatalf("protected file %q missing: %v", key, statErr)
			}
		}
	})

	t.Run("blob garbage collection loops batches", func(t *testing.T) {
		store := &blobs.Store{Dir: t.TempDir()}
		if _, err := database.ExecContext(ctx, `INSERT INTO blob_ledger
			(blob_key, user_id, size_bytes, state)
			SELECT $1::text || '/gc-batch-' || n, $1::uuid, 0, 'purgeable'
			FROM generate_series(1, 501) AS n`, userID); err != nil {
			t.Fatal(err)
		}
		result, err := jobs.GarbageCollectBlobs(ctx, database, store)
		if err != nil {
			t.Fatal(err)
		}
		if result.RowsPurged != 501 || result.FilesRemoved != 0 {
			t.Fatalf("unexpected batched GC result: %#v", result)
		}
		assertRowCount(t, database, `SELECT count(*) FROM blob_ledger
			WHERE user_id = $1 AND blob_key LIKE $2`, 0, userID, userID+"/gc-batch-%")
	})
}

func assertRowCount(t *testing.T, database *databasepkg.DB, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := database.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("row count = %d, want %d", got, want)
	}
}
