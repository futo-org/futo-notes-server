package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
)

// Schedule sets when Run fires its two job groups: hourly session reaping and
// the slower storage and mutation-result maintenance.
type Schedule struct {
	InitialDelay        time.Duration
	SessionInterval     time.Duration
	MaintenanceInterval time.Duration
}

// DefaultSchedule is the production schedule.
var DefaultSchedule = Schedule{
	InitialDelay:        time.Minute,
	SessionInterval:     time.Hour,
	MaintenanceInterval: 6 * time.Hour,
}

// Run executes recurring jobs until ctx is cancelled. Both schedules first
// fire after InitialDelay, and an error from one job never stops later jobs.
func Run(ctx context.Context, database *db.DB, store *blobs.Store, schedule Schedule, collectGarbage bool) {
	maintenance := maintenanceJobs(database, store, collectGarbage)

	run(ctx, schedule, scheduledJobs{
		sessions: []scheduledJob{{
			name: "sessions",
			run: func(ctx context.Context) (string, error) {
				result, err := ReapSessions(ctx, database)
				return fmt.Sprintf("reaped %d", result.Reaped), err
			},
		}},
		maintenance: maintenance,
	})
}

// maintenanceJobs lists the maintenance group, with blob GC only when
// collectGarbage is set.
func maintenanceJobs(database *db.DB, store *blobs.Store, collectGarbage bool) []scheduledJob {
	maintenance := []scheduledJob{
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
	}
	if collectGarbage {
		maintenance = append(maintenance, scheduledJob{
			name: "blob GC",
			run: func(ctx context.Context) (string, error) {
				result, err := GarbageCollectBlobs(ctx, database, store)
				return fmt.Sprintf("purged %d rows, removed %d files", result.RowsPurged, result.FilesRemoved), err
			},
		})
	}
	return maintenance
}

// scheduledJob is one named job. run returns a human-readable summary for the
// log, even on error.
type scheduledJob struct {
	name string
	run  func(context.Context) (string, error)
}

// scheduledJobs holds the two job groups Run fires on separate intervals.
type scheduledJobs struct {
	sessions    []scheduledJob
	maintenance []scheduledJob
}

// run is Run with the jobs injectable for tests.
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

// runJobs runs jobs in order, logging each outcome and continuing past
// failures.
func runJobs(ctx context.Context, jobs []scheduledJob) {
	for _, job := range jobs {
		summary, err := job.run(ctx)
		if err != nil {
			log.Error().Err(err).Str("job", job.name).Str("summary", summary).Msg("running job")
			continue
		}
		log.Info().Str("job", job.name).Str("summary", summary).Msg("ran job")
	}
}
