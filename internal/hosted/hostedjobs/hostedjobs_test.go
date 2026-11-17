package hostedjobs_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/hosted/hostedtest"
	"futo-notes-server/internal/jobs"
)

func TestLockedJobsRunOnOneReplicaAtATime(t *testing.T) {
	database := hostedtest.Postgres(t)
	ctx := t.Context()
	name := "lock test " + uuid.NewV7().String()
	dbtest.CleanupExec(t, database, `DELETE FROM hosted_job_runs WHERE name = $1`, name)
	runs := 0
	var recorded []error
	job := hostedjobs.Locked(database, []jobs.Job{{Name: name, Run: func(context.Context) (string, error) {
		runs++
		return "ran", errors.New("job failed")
	}}}, time.Hour, func(_ context.Context, job string, err error) {
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

// Two replicas, each with its own connection pool and scheduler, run a job
// at most once per interval between them, failed runs included.
func TestJobsRunOncePerIntervalAcrossReplicas(t *testing.T) {
	replicaA := hostedtest.Postgres(t)
	replicaB := hostedtest.Postgres(t)
	ctx := t.Context()
	name := "dedupe test " + uuid.NewV7().String()
	dbtest.CleanupExec(t, replicaA, `DELETE FROM hosted_job_runs WHERE name = $1`, name)

	var runs atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	body := []jobs.Job{{Name: name, Run: func(context.Context) (string, error) {
		runs.Add(1)
		started <- struct{}{}
		<-release
		return "ran", errors.New("job failed")
	}}}
	noRecord := func(context.Context, string, error) {}
	jobA := hostedjobs.Locked(replicaA, body, time.Hour, noRecord)[0]
	jobB := hostedjobs.Locked(replicaB, body, time.Hour, noRecord)[0]

	// B ticks while A is mid-run, then again after A finished.
	done := make(chan error, 1)
	go func() {
		_, err := jobA.Run(ctx)
		done <- err
	}()
	<-started
	if summary, err := jobB.Run(ctx); err != nil || !strings.Contains(summary, "another replica") {
		t.Fatalf("B during A's run = %q, %v; want skipped for the lock", summary, err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("A's run error was lost")
	}
	for _, job := range []jobs.Job{jobB, jobA} {
		if summary, err := job.Run(ctx); err != nil || !strings.Contains(summary, "recently") {
			t.Fatalf("run after A's = %q, %v; want skipped as recent", summary, err)
		}
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("job ran %d times within one interval, want 1", n)
	}

	// An interval later, whichever replica ticks first runs it, once.
	if _, err := replicaA.ExecContext(ctx, `UPDATE hosted_job_runs SET last_started_at = now() - interval '1 hour'
		WHERE name = $1`, name); err != nil {
		t.Fatal(err)
	}
	if summary, err := jobB.Run(ctx); summary != "ran" || err == nil {
		t.Fatalf("B an interval later = %q, %v; want it to run", summary, err)
	}
	if summary, _ := jobA.Run(ctx); !strings.Contains(summary, "recently") {
		t.Fatalf("A right after B = %q; want skipped as recent", summary)
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("job ran %d times over two intervals, want 2", n)
	}
}
