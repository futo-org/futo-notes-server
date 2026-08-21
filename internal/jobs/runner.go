package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"futo-notes-server/internal/blobs"
)

type Schedule struct {
	InitialDelay        time.Duration
	SessionInterval     time.Duration
	MaintenanceInterval time.Duration
}

var DefaultSchedule = Schedule{
	InitialDelay:        time.Minute,
	SessionInterval:     time.Hour,
	MaintenanceInterval: 6 * time.Hour,
}

// Run executes recurring jobs until ctx is cancelled. Both schedules first
// fire after InitialDelay, and an error from one job never stops later jobs.
func Run(ctx context.Context, database *sql.DB, store *blobs.Store, schedule Schedule) {
	run(ctx, schedule, scheduledJobs{
		sessions: []scheduledJob{{
			name: "sessions",
			run: func(ctx context.Context) (string, error) {
				result, err := ReapSessions(ctx, database)
				return fmt.Sprintf("reaped %d", result.Reaped), err
			},
		}},
		maintenance: []scheduledJob{
			{
				name: "storage reconciliation",
				run: func(ctx context.Context) (string, error) {
					result, err := ReconcileStorage(ctx, database, store)
					return fmt.Sprintf("adopted %d, skipped %d, cap hit %t", result.Adopted, result.Skipped, result.CapHit), err
				},
			},
			{
				name: "mutation results",
				run: func(ctx context.Context) (string, error) {
					result, err := ExpireMutationResults(ctx, database)
					return fmt.Sprintf("expired %d pending, %d other", result.PendingExpired, result.OtherExpired), err
				},
			},
			{
				name: "blob GC",
				run: func(ctx context.Context) (string, error) {
					result, err := GarbageCollectBlobs(ctx, database, store)
					return fmt.Sprintf("purged %d rows, removed %d files", result.RowsPurged, result.FilesRemoved), err
				},
			},
		},
	})
}

type scheduledJob struct {
	name string
	run  func(context.Context) (string, error)
}

type scheduledJobs struct {
	sessions    []scheduledJob
	maintenance []scheduledJob
}

func run(ctx context.Context, schedule Schedule, jobs scheduledJobs) {
	sessionTicker := time.NewTicker(schedule.InitialDelay)
	defer sessionTicker.Stop()
	maintenanceTicker := time.NewTicker(schedule.InitialDelay)
	defer maintenanceTicker.Stop()
	sessionFirst, maintenanceFirst := true, true

	for {
		select {
		case <-ctx.Done():
			return
		case <-sessionTicker.C:
			if sessionFirst {
				sessionTicker.Reset(schedule.SessionInterval)
				sessionFirst = false
			}
			runJobs(ctx, jobs.sessions)
		case <-maintenanceTicker.C:
			if maintenanceFirst {
				maintenanceTicker.Reset(schedule.MaintenanceInterval)
				maintenanceFirst = false
			}
			runJobs(ctx, jobs.maintenance)
		}
	}
}

func runJobs(ctx context.Context, jobs []scheduledJob) {
	for _, job := range jobs {
		summary, err := job.run(ctx)
		if err != nil {
			slog.Error(job.name, "summary", summary, "err", err)
			continue
		}
		slog.Info(job.name, "summary", summary)
	}
}
