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

	RunJobs(ctx, schedule, []Job{SessionReaping(database)}, maintenance)
}

// SessionReaping is the job that runs ReapSessions.
func SessionReaping(database *db.DB) Job {
	return Job{
		Name: "sessions",
		Run: func(ctx context.Context) (string, error) {
			result, err := ReapSessions(ctx, database)
			return fmt.Sprintf("reaped %d", result.Reaped), err
		},
	}
}

// MutationResultExpiry is the job that runs ExpireMutationResults.
func MutationResultExpiry(database *db.DB) Job {
	return Job{
		Name: "mutation results",
		Run: func(ctx context.Context) (string, error) {
			result, err := ExpireMutationResults(ctx, database)
			return fmt.Sprintf("expired %d pending, %d other", result.PendingExpired, result.OtherExpired), err
		},
	}
}

// maintenanceJobs lists the maintenance group, with blob GC only when
// collectGarbage is set.
func maintenanceJobs(database *db.DB, store *blobs.Store, collectGarbage bool) []Job {
	maintenance := []Job{
		{
			Name: "storage reconciliation",
			Run: func(ctx context.Context) (string, error) {
				result, err := ReconcileStorage(ctx, database, store)
				return fmt.Sprintf("adopted %d, skipped %d, cap hit %t", result.Adopted, result.Skipped, result.CapHit), err
			},
		},
		MutationResultExpiry(database),
	}
	if collectGarbage {
		maintenance = append(maintenance, Job{
			Name: "blob GC",
			Run: func(ctx context.Context) (string, error) {
				result, err := GarbageCollectBlobs(ctx, database, store)
				return fmt.Sprintf("purged %d rows, removed %d files", result.RowsPurged, result.FilesRemoved), err
			},
		})
	}
	return maintenance
}

// Job is one named job. Run returns a human-readable summary for the log, even
// on error.
type Job struct {
	Name string
	Run  func(context.Context) (string, error)
}

// RunJobs runs the session jobs every SessionInterval and the maintenance jobs
// every MaintenanceInterval, both first after InitialDelay, until ctx is
// cancelled. An error from one job never stops later jobs.
func RunJobs(ctx context.Context, schedule Schedule, sessions, maintenance []Job) {
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
			runJobs(ctx, sessions)
		case <-maintenanceTicker.C:
			if maintenanceFirst {
				maintenanceTicker.Reset(schedule.MaintenanceInterval)
				maintenanceFirst = false
			}
			runJobs(ctx, maintenance)
		}
	}
}

// runJobs runs jobs in order, logging each outcome and continuing past
// failures.
func runJobs(ctx context.Context, jobs []Job) {
	for _, job := range jobs {
		summary, err := job.Run(ctx)
		if err != nil {
			log.Error().Err(err).Str("job", job.Name).Str("summary", summary).Msg("running job")
			continue
		}
		log.Info().Str("job", job.Name).Str("summary", summary).Msg("ran job")
	}
}
