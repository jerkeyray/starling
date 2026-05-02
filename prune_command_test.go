package starling_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
)

func TestPruneCmd_DryRunThenConfirm(t *testing.T) {
	db, runID := seedSQLiteRun(t)

	var dry bytes.Buffer
	c := starling.PruneCommand()
	c.Output = &dry
	if err := c.Run([]string{"--before", "2260-01-01T00:00:00Z", db}); err != nil {
		t.Fatalf("dry Run: %v", err)
	}
	if !strings.Contains(dry.String(), "would delete 1 runs") {
		t.Fatalf("dry output = %q", dry.String())
	}
	if !strings.Contains(dry.String(), runID) {
		t.Fatalf("dry output missing run id: %q", dry.String())
	}

	log, err := eventlog.NewSQLite(db, eventlog.WithReadOnly())
	if err != nil {
		t.Fatalf("reopen read-only: %v", err)
	}
	evs, err := log.Read(context.Background(), runID)
	_ = log.Close()
	if err != nil {
		t.Fatalf("read after dry-run: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("dry-run deleted the run")
	}

	var confirmed bytes.Buffer
	c = starling.PruneCommand()
	c.Output = &confirmed
	if err := c.Run([]string{"--before", "2260-01-01T00:00:00Z", "--confirm", db}); err != nil {
		t.Fatalf("confirm Run: %v", err)
	}
	if !strings.Contains(confirmed.String(), "deleted 1 runs") {
		t.Fatalf("confirm output = %q", confirmed.String())
	}

	log, err = eventlog.NewSQLite(db, eventlog.WithReadOnly())
	if err != nil {
		t.Fatalf("reopen read-only after confirm: %v", err)
	}
	evs, err = log.Read(context.Background(), runID)
	_ = log.Close()
	if err != nil {
		t.Fatalf("read after confirm: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("confirm left %d events", len(evs))
	}
}

func TestPruneCmd_RequiresCutoff(t *testing.T) {
	db, _ := seedSQLiteRun(t)
	c := starling.PruneCommand()
	c.Output = &bytes.Buffer{}
	if err := c.Run([]string{db}); err == nil {
		t.Fatal("expected missing cutoff error")
	}
}

func TestPruneCmd_RejectsConflictingCutoffs(t *testing.T) {
	db, _ := seedSQLiteRun(t)
	c := starling.PruneCommand()
	c.Output = &bytes.Buffer{}
	err := c.Run([]string{"--older-than", time.Hour.String(), "--before", "2260-01-01T00:00:00Z", db})
	if err == nil {
		t.Fatal("expected conflicting cutoff error")
	}
}
