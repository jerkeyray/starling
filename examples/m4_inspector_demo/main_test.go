package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/eventlog"
)

func TestSeededTerminalRunsValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.db")
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	seedHappyRun(ctx, log, "demo-completed", now.Add(-10*time.Minute))
	seedFailedRun(ctx, log, "demo-failed", now.Add(-7*time.Minute))
	seedCancelledRun(ctx, log, "demo-cancelled", now.Add(-4*time.Minute))
	seedInProgressRun(ctx, log, "demo-in-progress", now.Add(-1*time.Minute))

	for _, runID := range []string{"demo-completed", "demo-failed", "demo-cancelled"} {
		t.Run(runID, func(t *testing.T) {
			evs, err := log.Read(ctx, runID)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if err := eventlog.Validate(evs); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}

	evs, err := log.Read(ctx, "demo-in-progress")
	if err != nil {
		t.Fatalf("Read in-progress: %v", err)
	}
	err = eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) || !strings.Contains(err.Error(), "not terminal") {
		t.Fatalf("Validate in-progress = %v, want non-terminal ErrLogCorrupt", err)
	}
}
