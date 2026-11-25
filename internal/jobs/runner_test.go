package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"futo-notes-server/internal/blobs"
)

func TestReconcileStorageMissingDirectoryIsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	result, err := ReconcileStorage(t.Context(), nil, &blobs.Store{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if result != (ReconciliationResult{}) {
		t.Fatalf("result = %#v, want zero", result)
	}
}

func TestBlobGCRegistrationCanBeDisabled(t *testing.T) {
	for _, test := range []struct {
		name         string
		enabled      bool
		wantJobCount int
		wantLastJob  string
	}{
		{name: "enabled", enabled: true, wantJobCount: 3, wantLastJob: "blob GC"},
		{name: "disabled", enabled: false, wantJobCount: 2, wantLastJob: "mutation results"},
	} {
		t.Run(test.name, func(t *testing.T) {
			jobs := maintenanceJobs(nil, nil, test.enabled)
			if len(jobs) != test.wantJobCount {
				t.Fatalf("job count = %d, want %d", len(jobs), test.wantJobCount)
			}
			if got := jobs[len(jobs)-1].Name; got != test.wantLastJob {
				t.Fatalf("last job = %q, want %q", got, test.wantLastJob)
			}
		})
	}
}

func TestRunnerRepeatsAndContinuesAfterJobErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var sessionRuns, failingMaintenanceRuns, laterMaintenanceRuns atomic.Int32
		jobError := errors.New("injected job failure")
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunJobs(ctx, Schedule{
				InitialDelay:        time.Minute,
				SessionInterval:     time.Hour,
				MaintenanceInterval: 6 * time.Hour,
			}, []Job{{Name: "sessions", Run: func(context.Context) (string, error) {
				if sessionRuns.Add(1) == 1 {
					return "first run", jobError
				}
				return "later run", nil
			}}},
				[]Job{
					{Name: "failing maintenance", Run: func(context.Context) (string, error) {
						failingMaintenanceRuns.Add(1)
						return "failed", jobError
					}},
					{Name: "later maintenance", Run: func(context.Context) (string, error) {
						laterMaintenanceRuns.Add(1)
						return "ran", nil
					}},
				},
			)
		}()

		assertRuns := func(when string, sessions, maintenance int32) {
			t.Helper()
			synctest.Wait()
			if got := sessionRuns.Load(); got != sessions {
				t.Fatalf("%s: session runs = %d, want %d", when, got, sessions)
			}
			if got := failingMaintenanceRuns.Load(); got != maintenance {
				t.Fatalf("%s: failing maintenance runs = %d, want %d", when, got, maintenance)
			}
			if got := laterMaintenanceRuns.Load(); got != maintenance {
				t.Fatalf("%s: later maintenance runs = %d, want %d", when, got, maintenance)
			}
		}

		assertRuns("at start", 0, 0)
		time.Sleep(time.Minute)
		assertRuns("after the initial delay", 1, 1)
		time.Sleep(time.Hour)
		assertRuns("after one session interval", 2, 1)
		time.Sleep(5 * time.Hour)
		assertRuns("after one maintenance interval", 7, 2)

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("runner did not stop after cancellation")
		}
	})
}
