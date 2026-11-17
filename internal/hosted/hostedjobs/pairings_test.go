package hostedjobs_test

import (
	"testing"
	"uuid"

	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/hosted/hostedtest"
	"futo-notes-server/internal/hosted/pairing"
)

// The hourly group sweeps pairings past their window, keyed or not, and
// leaves live ones.
func TestSessionsGroupSweepsExpiredPairings(t *testing.T) {
	database := hostedtest.Postgres(t)
	ctx := t.Context()
	userID := uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email) VALUES ($1, $2, 'jobs test', $3)`,
		userID, "jobs-test-"+userID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, userID)

	offer := pairing.Offer{PublicKey: "pk", DeviceName: "phone", Platform: "ios"}
	var ids []string
	for range 3 {
		id, _, err := pairing.Create(ctx, database, userID, offer)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := pairing.Deliver(ctx, database, userID, ids[1], "sealed"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE pairings SET expires_at = now() - interval '1 second' WHERE id IN ($1, $2)`,
		ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}

	var ran bool
	for _, job := range hostedjobs.Sessions(database, account.New(database, make([]byte, 32))) {
		if job.Name != "pairings" {
			continue
		}
		ran = true
		if summary, err := job.Run(ctx); err != nil {
			t.Fatalf("pairings job = %q, %v", summary, err)
		}
	}
	if !ran {
		t.Fatal("the hourly group has no pairings job")
	}
	var left []string
	rows, err := database.QueryContext(ctx, `SELECT id FROM pairings WHERE user_id = $1`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if len(left) != 1 || left[0] != ids[2] {
		t.Fatalf("pairings after the sweep = %v, want only %s", left, ids[2])
	}
}
