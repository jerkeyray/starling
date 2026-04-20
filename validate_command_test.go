package starling_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
)

// seedSQLiteRun runs a single-turn agent against a fresh on-disk
// SQLite log and returns the db path + runID. Used by all three
// *_command_test.go files that need a real, replay-shaped log.
func seedSQLiteRun(t *testing.T) (dbPath string, runID string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "runs.db")
	log, err := eventlog.NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "hello"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 2},
	}
	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return dbPath, res.RunID
}

func TestValidateCmd_OK_SingleRun(t *testing.T) {
	db, runID := seedSQLiteRun(t)

	var buf bytes.Buffer
	c := starling.ValidateCommand()
	c.Output = &buf
	if err := c.Run([]string{db, runID}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, runID+"\tOK") {
		t.Errorf("output = %q, want line with %q\\tOK", got, runID)
	}
}

func TestValidateCmd_OK_AllRuns(t *testing.T) {
	db, runID := seedSQLiteRun(t)

	var buf bytes.Buffer
	c := starling.ValidateCommand()
	c.Output = &buf
	if err := c.Run([]string{db}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), runID+"\tOK") {
		t.Errorf("output = %q, want %q\\tOK", buf.String(), runID)
	}
}

func TestValidateCmd_UnknownRun(t *testing.T) {
	db, _ := seedSQLiteRun(t)

	var buf bytes.Buffer
	c := starling.ValidateCommand()
	c.Output = &buf
	err := c.Run([]string{db, "nope"})
	if err == nil {
		t.Fatal("expected error for unknown runID")
	}
	if !strings.Contains(buf.String(), "ERROR") {
		t.Errorf("output = %q, want ERROR line", buf.String())
	}
}

func TestValidateCmd_BadArgs(t *testing.T) {
	var buf bytes.Buffer
	c := starling.ValidateCommand()
	c.Output = &buf
	if err := c.Run(nil); err == nil {
		t.Fatal("expected error on missing <db>")
	}
}

func TestValidateCmd_MissingDB(t *testing.T) {
	var buf bytes.Buffer
	c := starling.ValidateCommand()
	c.Output = &buf
	if err := c.Run([]string{filepath.Join(t.TempDir(), "nope.db")}); err == nil {
		t.Fatal("expected error for missing db")
	}
}
