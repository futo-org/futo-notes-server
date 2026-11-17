// Package hostedjobs runs the hosted API's background jobs. Every replica runs
// the scheduler; each run first takes a Postgres advisory lock for its job,
// so a given job never runs on two replicas at once.
//
// Blob garbage collection, orphan reconciliation and account purge act on the
// regional storage services and are added to Maintenance with them. The
// self-hosted filesystem jobs (internal/jobs ReconcileStorage and
// GarbageCollectBlobs) must never run here: the API holds no blob files.
package hostedjobs

import (
	"context"
	"fmt"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/jobs"
)

// RecordRun observes one run of a job that held its lock.
type RecordRun func(ctx context.Context, job string, err error)

// Sessions is the hourly job group: expired sessions (and with them their
// storage refresh tokens) and abandoned logins.
func Sessions(database *db.DB, accounts *account.Store) []jobs.Job {
	return []jobs.Job{
		jobs.SessionReaping(database),
		{
			Name: "oidc logins",
			Run: func(ctx context.Context) (string, error) {
				deleted, err := accounts.DeleteExpiredLogins(ctx)
				return fmt.Sprintf("deleted %d", deleted), err
			},
		},
	}
}

// Maintenance is the slower job group.
func Maintenance(database *db.DB) []jobs.Job {
	return []jobs.Job{jobs.MutationResultExpiry(database)}
}

// Run runs the job groups on schedule until ctx is cancelled, each job under
// its advisory lock.
func Run(ctx context.Context, database *db.DB, schedule jobs.Schedule, sessions, maintenance []jobs.Job, record RecordRun) {
	jobs.RunJobs(ctx, schedule, Locked(database, sessions, record), Locked(database, maintenance, record))
}

// Locked wraps each job so that a run first takes the job's
// pg_try_advisory_xact_lock and is skipped when another replica holds it.
// The lock is held by a transaction left open for the run, so it is released
// when the run ends or its connection dies.
func Locked(database *db.DB, list []jobs.Job, record RecordRun) []jobs.Job {
	locked := make([]jobs.Job, 0, len(list))
	for _, job := range list {
		locked = append(locked, jobs.Job{
			Name: job.Name,
			Run: func(ctx context.Context) (string, error) {
				tx, err := database.BeginTx(ctx, nil)
				if err != nil {
					return "", fmt.Errorf("taking job lock: %w", err)
				}
				defer tx.Discard()
				var acquired bool
				if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`,
					"notes-api job: "+job.Name).Scan(&acquired); err != nil {
					return "", fmt.Errorf("taking job lock: %w", err)
				}
				if !acquired {
					return "skipped: another replica is running it", nil
				}
				summary, err := job.Run(ctx)
				record(ctx, job.Name, err)
				return summary, err
			},
		})
	}
	return locked
}
