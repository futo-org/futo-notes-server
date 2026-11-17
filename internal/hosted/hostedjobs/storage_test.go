package hostedjobs_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"go.opentelemetry.io/otel/metric/noop"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/hosted/hostedtest"
	"futo-notes-server/internal/hosted/storageclient"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/objects"
	"futo-notes-server/internal/storagesvc/adminapi"
	"futo-notes-server/internal/storagesvc/handlers"
	"futo-notes-server/internal/storagesvc/metrics"
	"futo-notes-server/internal/storagesvc/storage"
)

const testRegion = "us-east"

// testStorage is the MinIO bucket seen through a namespace of its own, so a
// test's jobs, which list and delete across the whole bucket, never touch
// other tests' objects or a developer's. deleteBudget, when not negative, is
// how many more keys deletes may remove before they fail.
type testStorage struct {
	inner        storage.Storage
	ns           string
	deleteBudget atomic.Int64
}

func (s *testStorage) PutObject(ctx context.Context, key string, body io.Reader, size int64) error {
	return s.inner.PutObject(ctx, s.ns+key, body, size)
}

func (s *testStorage) GetObject(ctx context.Context, key, byteRange string) (*storage.Object, error) {
	return s.inner.GetObject(ctx, s.ns+key, byteRange)
}

func (s *testStorage) ListObjects(ctx context.Context, prefix, startAfter string, limit int) (storage.ListPage, error) {
	if startAfter != "" {
		startAfter = s.ns + startAfter
	}
	page, err := s.inner.ListObjects(ctx, s.ns+prefix, startAfter, limit)
	for i := range page.Objects {
		page.Objects[i].Key = strings.TrimPrefix(page.Objects[i].Key, s.ns)
	}
	page.LastKey = strings.TrimPrefix(page.LastKey, s.ns)
	return page, err
}

func (s *testStorage) DeleteObjects(ctx context.Context, keys []string) ([]storage.DeleteError, error) {
	allowed := len(keys)
	if budget := s.deleteBudget.Load(); budget >= 0 {
		allowed = min(allowed, int(budget))
		s.deleteBudget.Add(-int64(allowed))
	}
	inner := make([]string, allowed)
	for i, key := range keys[:allowed] {
		inner[i] = s.ns + key
	}
	failed, err := s.inner.DeleteObjects(ctx, inner)
	for i := range failed {
		failed[i].Key = strings.TrimPrefix(failed[i].Key, s.ns)
	}
	if err == nil && allowed < len(keys) {
		err = errors.New("simulated storage failure")
	}
	return failed, err
}

// storageEnv is the API database and a us-east storage service running the
// real handlers in-process over MinIO, with a storage client for it.
type storageEnv struct {
	t        *testing.T
	database *db.DB
	store    *testStorage
	client   *storageclient.Client
}

func newStorageEnv(t *testing.T) *storageEnv {
	t.Helper()
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT not set")
	}
	s3Region := os.Getenv("S3_TEST_REGION")
	if s3Region == "" {
		s3Region = "us-east-1"
	}
	database := hostedtest.Postgres(t)
	store := &testStorage{
		inner: storage.NewS3Storage(storage.S3Options{
			Endpoint:        endpoint,
			Region:          s3Region,
			Bucket:          "notes-us-east",
			AccessKeyID:     os.Getenv("S3_TEST_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("S3_TEST_SECRET_ACCESS_KEY"),
			ForcePathStyle:  true,
		}),
		ns: "hostedjobs-test-" + uuid.NewV7().String() + "/",
	}
	store.deleteBudget.Store(-1)
	t.Cleanup(func() {
		ctx := context.Background()
		for {
			page, err := store.inner.ListObjects(ctx, store.ns, "", adminapi.MaxDeleteKeys)
			if err != nil {
				t.Errorf("cleanup list: %v", err)
				return
			}
			keys := make([]string, len(page.Objects))
			for i, o := range page.Objects {
				keys[i] = o.Key
			}
			if failed, err := store.inner.DeleteObjects(ctx, keys); err != nil || len(failed) > 0 {
				t.Errorf("cleanup delete: %v %v", failed, err)
				return
			}
			if !page.Truncated {
				return
			}
		}
	})

	apiKey := generateKey(t, "test-api")
	regionKey := generateKey(t, "test-"+testRegion)
	verifier, err := hostedauth.NewVerifier(publicKeys(t, apiKey), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := hostedauth.NewReceiptSigner(regionKey, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	meter := noop.NewMeterProvider().Meter("test")
	storageMetrics, err := metrics.NewMetrics(meter)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&handlers.Server{Storage: store, Verifier: verifier, Signer: signer, Metrics: storageMetrics}).Handler())
	t.Cleanup(srv.Close)
	client, err := storageclient.New([]config.Region{{ID: testRegion, Endpoint: srv.URL}},
		hostedauth.NewMinter(apiKey), meter)
	if err != nil {
		t.Fatal(err)
	}
	return &storageEnv{t: t, database: database, store: store, client: client}
}

func generateKey(t *testing.T, kid string) hostedauth.SigningKey {
	t.Helper()
	key, err := hostedauth.GenerateSigningKey(kid)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func publicKeys(t *testing.T, key hostedauth.SigningKey) hostedauth.KeySet {
	t.Helper()
	public, err := key.MarshalPublicPEM()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := hostedauth.ParseKeySet(public)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// user inserts a user in region ("" for none) and removes it when the test
// ends, if a job has not.
func (e *storageEnv) user(region string) string {
	e.t.Helper()
	id := uuid.NewV7().String()
	var regionID *string
	if region != "" {
		regionID = &region
	}
	if _, err := e.database.ExecContext(e.t.Context(), `INSERT INTO users (id, sub, name, email, region_id)
		VALUES ($1, $2, 'jobs test', $3, $4)`, id, "jobs-test-"+id, id+"@example.test", regionID); err != nil {
		e.t.Fatal(err)
	}
	dbtest.CleanupExec(e.t, e.database, `DELETE FROM users WHERE id = $1`, id)
	return id
}

// upload stores a one-byte blob for userID and returns its key.
func (e *storageEnv) upload(userID string) string {
	e.t.Helper()
	key := userID + "/" + uuid.NewV7().String()
	if err := e.store.PutObject(e.t.Context(), key, strings.NewReader("x"), 1); err != nil {
		e.t.Fatal(err)
	}
	return key
}

// ledger records key in the ledger in state, changed age ago.
func (e *storageEnv) ledger(userID, key, state string, age time.Duration) {
	e.t.Helper()
	if _, err := e.database.ExecContext(e.t.Context(), `INSERT INTO blob_ledger
		(blob_key, user_id, size_bytes, state, created_at, state_changed_at) VALUES ($1, $2, 1, $3, $4, $4)`,
		key, userID, state, time.Now().Add(-age)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *storageEnv) stored(key string) bool {
	e.t.Helper()
	obj, err := e.store.GetObject(e.t.Context(), key, "")
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	obj.Body.Close()
	return true
}

func (e *storageEnv) inLedger(key string) bool {
	e.t.Helper()
	var known bool
	if err := e.database.QueryRowContext(e.t.Context(), `SELECT EXISTS (SELECT 1 FROM blob_ledger WHERE blob_key = $1)`, key).
		Scan(&known); err != nil {
		e.t.Fatal(err)
	}
	return known
}

// lastModified returns when storage last wrote key.
func (e *storageEnv) lastModified(key string) time.Time {
	e.t.Helper()
	owner, _, _ := strings.Cut(key, "/")
	page, err := e.store.ListObjects(e.t.Context(), owner+"/", "", adminapi.MaxDeleteKeys)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, o := range page.Objects {
		if o.Key == key {
			return o.LastModified
		}
	}
	e.t.Fatalf("%s is not stored", key)
	return time.Time{}
}

func (e *storageEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.database.QueryRowContext(e.t.Context(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestBlobGCDeletesExpiredBlobsFromStorage(t *testing.T) {
	e := newStorageEnv(t)
	ctx := t.Context()
	userID := e.user(testRegion)
	keys := map[string]string{}
	for name, row := range map[string]struct {
		state string
		age   time.Duration
	}{
		"old retained":    {"retained", 366 * 24 * time.Hour},
		"old purgeable":   {"purgeable", hostedauth.ReceiptTTL + time.Hour},
		"young purgeable": {"purgeable", hostedauth.ReceiptTTL - time.Hour},
		"young retained":  {"retained", 364 * 24 * time.Hour},
		"old claimed":     {"claimed", 400 * 24 * time.Hour},
	} {
		keys[name] = e.upload(userID)
		e.ledger(userID, keys[name], row.state, row.age)
	}

	result, err := hostedjobs.CollectBlobs(ctx, e.database, e.client)
	if err != nil || result.RowsPurged < 2 || result.KeysDeleted < 2 || result.KeysFailed != 0 {
		t.Fatalf("GC = %+v, %v", result, err)
	}
	for name, kept := range map[string]bool{
		"old retained": false, "old purgeable": false,
		"young retained": true, "old claimed": true,
		"young purgeable": true, // its receipt may still be valid
	} {
		if e.stored(keys[name]) != kept || e.inLedger(keys[name]) != kept {
			t.Fatalf("%s: stored %t, in ledger %t; want both %t", name, e.stored(keys[name]), e.inLedger(keys[name]), kept)
		}
	}

	// A storage failure leaves the key stored but forgotten by the ledger;
	// reconciliation deletes it once it is old enough.
	stranded := e.upload(userID)
	e.ledger(userID, stranded, "purgeable", hostedauth.ReceiptTTL+time.Hour)
	e.store.deleteBudget.Store(0)
	if _, err := hostedjobs.CollectBlobs(ctx, e.database, e.client); err == nil {
		t.Fatal("GC with storage failing reported success")
	}
	e.store.deleteBudget.Store(-1)
	if !e.stored(stranded) || e.inLedger(stranded) {
		t.Fatalf("after a failed storage delete: stored %t, in ledger %t; want stored and forgotten", e.stored(stranded), e.inLedger(stranded))
	}
	later := e.lastModified(stranded).Add(hostedjobs.OrphanMinAge + time.Second)
	if _, err := hostedjobs.ReconcileOrphans(ctx, e.database, e.client, testRegion, "", later); err != nil {
		t.Fatal(err)
	}
	if e.stored(stranded) {
		t.Fatal("reconciliation kept a blob GC forgot")
	}
}

func TestReconciliationDeletesOnlyOldOrphans(t *testing.T) {
	e := newStorageEnv(t)
	ctx := t.Context()
	userID := e.user(testRegion)
	known := map[string]string{}
	for _, state := range []string{"claimed", "retained", "purgeable", "staged"} {
		known[state] = e.upload(userID)
		e.ledger(userID, known[state], state, 0)
	}
	oldOrphan := e.upload(userID)
	// S3 timestamps are coarse; keep the young orphan clearly younger.
	time.Sleep(1500 * time.Millisecond)
	youngOrphan := e.upload(userID)
	now := e.lastModified(oldOrphan).Add(hostedjobs.OrphanMinAge + 500*time.Millisecond)
	if !e.lastModified(youngOrphan).After(now.Add(-hostedjobs.OrphanMinAge)) {
		t.Fatal("young orphan is not younger than the cutoff")
	}

	result, err := hostedjobs.ReconcileOrphans(ctx, e.database, e.client, testRegion, "", now)
	if err != nil || result.Listed != len(known)+1 || result.Deleted != 1 || result.Failed != 0 || result.Skipped != 0 {
		t.Fatalf("reconciliation = %+v, %v", result, err)
	}
	if e.stored(oldOrphan) {
		t.Fatal("old orphan kept")
	}
	if !e.stored(youngOrphan) {
		t.Fatal("young orphan deleted")
	}
	for state, key := range known {
		if !e.stored(key) {
			t.Fatalf("deleted a %s key the ledger knows", state)
		}
	}
}

func TestAccountPurgeEmptiesStorageAndRowsAndResumes(t *testing.T) {
	e := newStorageEnv(t)
	ctx := t.Context()
	due, notDue, noRegion := e.user(testRegion), e.user(testRegion), e.user("")
	accounts := account.New(e.database)
	objectStore := objects.New(e.database, events.PostgresPublisher{})
	for _, userID := range []string{due, notDue, noRegion} {
		if err := accounts.RequestDeletion(ctx, userID, uuid.NewV7().String()); err != nil {
			t.Fatal(err)
		}
		if _, err := accounts.CreateSession(ctx, userID, "jobs test"); err != nil {
			t.Fatal(err)
		}
	}
	collectionID := uuid.NewV7().String()
	if _, err := e.database.ExecContext(ctx, `INSERT INTO collections (id, user_id) VALUES ($1, $2)`, collectionID, due); err != nil {
		t.Fatal(err)
	}
	if _, err := objectStore.Create(ctx, objects.Mutation{UserID: due, CollectionID: collectionID, MutationID: "purge-test"},
		objects.ReceiptBlob(e.upload(due), 1)); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		e.upload(due)
	}
	notDueKey := e.upload(notDue)
	if _, err := e.database.ExecContext(ctx, `UPDATE users SET purge_at = now() - interval '1 minute' WHERE id IN ($1, $2)`,
		due, noRegion); err != nil {
		t.Fatal(err)
	}
	rows := func(userID string) int {
		t.Helper()
		return e.count(`SELECT (SELECT count(*) FROM users WHERE id = $1) + (SELECT count(*) FROM sessions WHERE user_id = $1)
			+ (SELECT count(*) FROM collections WHERE user_id = $1) + (SELECT count(*) FROM objects WHERE user_id = $1)
			+ (SELECT count(*) FROM blob_ledger WHERE user_id = $1) + (SELECT count(*) FROM mutation_results WHERE user_id = $1)`, userID)
	}
	storedFor := func(userID string) int {
		t.Helper()
		page, err := e.store.ListObjects(ctx, userID+"/", "", adminapi.MaxDeleteKeys)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Objects)
	}
	if rows(due) != 6 || storedFor(due) != 5 {
		t.Fatalf("seeded %d rows and %d blobs", rows(due), storedFor(due))
	}

	// Storage fails after deleting two of the user's blobs: nothing of theirs
	// leaves the database, so the next run starts over.
	e.store.deleteBudget.Store(2)
	result, err := hostedjobs.PurgeAccounts(ctx, e.database, e.client)
	if err == nil || result.Failed < 1 || !strings.Contains(err.Error(), due) {
		t.Fatalf("purge with storage failing = %+v, %v", result, err)
	}
	if rows(due) != 6 || storedFor(due) != 3 {
		t.Fatalf("after a failed purge: %d rows, %d blobs; want 6 rows and 3 blobs", rows(due), storedFor(due))
	}
	if err := accounts.Restore(ctx, due); !errors.Is(err, account.ErrPurgeStarted) {
		t.Fatalf("restore mid-purge = %v, want ErrPurgeStarted", err)
	}
	if rows(noRegion) != 0 {
		t.Fatal("a due user with no region was not purged")
	}

	e.store.deleteBudget.Store(-1)
	for run := range 2 {
		if result, err := hostedjobs.PurgeAccounts(ctx, e.database, e.client); err != nil {
			t.Fatalf("purge run %d = %+v, %v", run, result, err)
		}
		if rows(due) != 0 || storedFor(due) != 0 {
			t.Fatalf("after purge run %d: %d rows, %d blobs left", run, rows(due), storedFor(due))
		}
	}
	if rows(notDue) != 2 || !e.stored(notDueKey) {
		t.Fatalf("an account still in its grace period lost data: %d rows, blob stored %t", rows(notDue), e.stored(notDueKey))
	}
}
