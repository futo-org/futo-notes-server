package hostedjobs_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/hosted/hostedtest"
	"futo-notes-server/internal/jobs"
)

func TestLockedJobsRunOnOneReplicaAtATime(t *testing.T) {
	database := hostedtest.Postgres(t)
	ctx := t.Context()
	name := "lock test " + uuid.NewV7().String()
	runs := 0
	var recorded []error
	job := hostedjobs.Locked(database, []jobs.Job{{Name: name, Run: func(context.Context) (string, error) {
		runs++
		return "ran", errors.New("job failed")
	}}}, func(_ context.Context, job string, err error) {
		if job != name {
			t.Errorf("recorded job %q, want %q", job, name)
		}
		recorded = append(recorded, err)
	})[0]

	// Another replica holds the job's lock.
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "notes-api job: "+name); err != nil {
		t.Fatal(err)
	}
	summary, err := job.Run(ctx)
	if err != nil || runs != 0 || len(recorded) != 0 {
		t.Fatalf("run while locked = %q, %v, %d runs; want skipped", summary, err, runs)
	}
	tx.Discard()

	summary, err = job.Run(ctx)
	if summary != "ran" || err == nil || runs != 1 || len(recorded) != 1 || recorded[0] == nil {
		t.Fatalf("run when free = %q, %v, %d runs, recorded %v", summary, err, runs, recorded)
	}
}
