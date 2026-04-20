package starling_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
)

func TestExportCmd_NDJSONRoundTrips(t *testing.T) {
	db, runID := seedSQLiteRun(t)

	var buf bytes.Buffer
	c := starling.ExportCommand()
	c.Output = &buf
	if err := c.Run([]string{db, runID}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("no output lines")
	}

	// Each line must be valid JSON with the expected envelope.
	type envelope struct {
		RunID   string          `json:"run_id"`
		Seq     uint64          `json:"seq"`
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	var kinds []string
	for i, line := range lines {
		var env envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("line %d: unmarshal: %v; line=%q", i, err, line)
		}
		if env.RunID != runID {
			t.Errorf("line %d: run_id = %q, want %q", i, env.RunID, runID)
		}
		if env.Seq != uint64(i+1) {
			t.Errorf("line %d: seq = %d, want %d", i, env.Seq, i+1)
		}
		if len(env.Payload) == 0 {
			t.Errorf("line %d: empty payload", i)
		}
		kinds = append(kinds, env.Kind)
	}

	want := []string{
		event.KindRunStarted.String(),
		event.KindTurnStarted.String(),
		event.KindAssistantMessageCompleted.String(),
		event.KindRunCompleted.String(),
	}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v\n want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kinds[%d] = %q, want %q", i, kinds[i], want[i])
		}
	}
}

func TestExportCmd_UnknownRun(t *testing.T) {
	db, _ := seedSQLiteRun(t)
	c := starling.ExportCommand()
	c.Output = &bytes.Buffer{}
	if err := c.Run([]string{db, "nope"}); err == nil {
		t.Fatal("expected error for unknown runID")
	}
}

func TestExportCmd_BadArgs(t *testing.T) {
	c := starling.ExportCommand()
	c.Output = &bytes.Buffer{}
	if err := c.Run([]string{filepath.Join(t.TempDir(), "x.db")}); err == nil {
		t.Fatal("expected error for missing runID")
	}
}
