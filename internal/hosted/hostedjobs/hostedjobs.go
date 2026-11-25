// Package hostedjobs runs the hosted API's background jobs. Every replica runs
// the scheduler; each run first takes a Postgres advisory lock for its job,
// so a given job never runs on two replicas at once, and checks when the job
// last started anywhere, so it runs at most once per interval across them.
//
// The self-hosted filesystem jobs (internal/jobs ReconcileStorage and
// GarbageCollectBlobs) must never run here: the API holds no blob files.
// Blob GC, orphan reconciliation and account purge act on the regional
// storage services instead. The billing jobs reconcile subscriptions with
// their providers and move lapsed accounts toward their purge.
package hostedjobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hosted/pairing"
	"futo-notes-server/internal/jobs"
)

// RecordRun observes one run of a job that held its lock.
type RecordRun func(ctx context.Context, job string, err error)

// Sessions is the hourly job group: expired sessions (and with them their
// storage refresh tokens), abandoned logins, and expired pairings, whose
// sealed vault keys should not outlive their window by long.
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
		{
			Name: "pairings",
			Run: func(ctx context.Context) (string, error) {
				deleted, err := pairing.DeleteExpired(ctx, database)
				return fmt.Sprintf("deleted %d", deleted), err
			},
		},
	}
}

// Maintenance is the slower job group, every 6 hours: mutation result
// expiry, the jobs that act on the storage services of regions (every
// configured region id), and billing: entitlement reconciliation, then the
// lapse job, which purges the lapsed accounts it claims, then the account
// purge, which finishes any it could not, and delivery log pruning.
func Maintenance(b Billing, regions []string, metrics *Metrics) []jobs.Job {
	return []jobs.Job{
		jobs.MutationResultExpiry(b.Database),
		blobGC(b.Database, b.Storage, metrics),
		orphanReconciliation(b.Database, b.Storage, regions, metrics),
		entitlementReconciliation(b, metrics),
		subscriptionLapses(b, metrics),
		accountPurge(b, metrics),
		deliveryPruning(b, metrics),
	}
}

// Run runs the job groups on schedule until ctx is cancelled, each job under
// its advisory lock and at most once per its group's interval.
func Run(ctx context.Context, database *db.DB, schedule jobs.Schedule, sessions, maintenance []jobs.Job, record RecordRun) {
	jobs.RunJobs(ctx, schedule,
		locked(database, sessions, schedule.SessionInterval, record),
		locked(database, maintenance, schedule.MaintenanceInterval, record))
}

// dueAfter is how long after a job last started it may start again. Each
// replica's ticks drift by the time the earlier jobs of the group took, so
// requiring the full interval would make the replica that ran a job skip its
// own next, on-time run and leave it to whichever replica ticks later.
func dueAfter(interval time.Duration) time.Duration { return interval * 9 / 10 }

// locked wraps each job so that a run first takes the job's
// pg_try_advisory_xact_lock, and is skipped when another replica holds it or
// when the job started less than about interval ago on any replica. The lock
// is held by a transaction left open for the run, so it is released when the
// run ends or its connection dies; the start is recorded in the same
// transaction, so a run whose replica dies is not counted.
func locked(database *db.DB, list []jobs.Job, interval time.Duration, record RecordRun) []jobs.Job {
	wrapped := make([]jobs.Job, 0, len(list))
	for _, job := range list {
		wrapped = append(wrapped, jobs.Job{
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
				// The database clock, so every replica measures the interval
				// the same way.
				var due int
				err = tx.QueryRowContext(ctx, `INSERT INTO hosted_job_runs (name, last_started_at) VALUES ($1, now())
					ON CONFLICT (name) DO UPDATE SET last_started_at = now()
					WHERE hosted_job_runs.last_started_at <= now() - make_interval(secs => $2)
					RETURNING 1`, job.Name, dueAfter(interval).Seconds()).Scan(&due)
				if errors.Is(err, sql.ErrNoRows) {
					return "skipped: it ran recently", nil
				}
				if err != nil {
					return "", fmt.Errorf("checking the job's last run: %w", err)
				}
				summary, err := job.Run(ctx)
				record(ctx, job.Name, err)
				if commitErr := tx.Commit(); commitErr != nil {
					err = errors.Join(err, fmt.Errorf("recording the job's run: %w", commitErr))
				}
				return summary, err
			},
		})
	}
	return wrapped
}
