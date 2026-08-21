package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"futo-notes-server/internal/blobs"
)

func TestReconcileStorageMissingDirectoryIsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	result, err := ReconcileStorage(context.Background(), nil, &blobs.Store{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if result != (ReconciliationResult{}) {
		t.Fatalf("result = %#v, want zero", result)
	}
}

func TestRunnerRepeatsAndContinuesAfterJobErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sessionRuns, failingMaintenanceRuns, laterMaintenanceRuns atomic.Int32
	jobError := errors.New("injected job failure")
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, Schedule{
			InitialDelay:        5 * time.Millisecond,
			SessionInterval:     5 * time.Millisecond,
			MaintenanceInterval: 5 * time.Millisecond,
		}, scheduledJobs{
			sessions: []scheduledJob{{name: "sessions", run: func(context.Context) (string, error) {
				run := sessionRuns.Add(1)
				if run == 1 {
					return "first run", jobError
				}
				if run >= 3 && laterMaintenanceRuns.Load() >= 2 {
					cancel()
				}
				return "later run", nil
			}}},
			maintenance: []scheduledJob{
				{name: "failing maintenance", run: func(context.Context) (string, error) {
					failingMaintenanceRuns.Add(1)
					return "failed", jobError
				}},
				{name: "later maintenance", run: func(context.Context) (string, error) {
					run := laterMaintenanceRuns.Add(1)
					if run >= 2 && sessionRuns.Load() >= 3 {
						cancel()
					}
					return "ran", nil
				}},
			},
		})
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("runner did not repeat on the compressed schedule")
	}

	if got := sessionRuns.Load(); got < 3 {
		t.Fatalf("session runs = %d, want at least 3", got)
	}
	if got := failingMaintenanceRuns.Load(); got < 2 {
		t.Fatalf("failing maintenance runs = %d, want at least 2", got)
	}
	if got := laterMaintenanceRuns.Load(); got < 2 {
		t.Fatalf("later maintenance runs = %d, want at least 2", got)
	}
}
