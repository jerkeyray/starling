package starling_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/replay"
	"github.com/jerkeyray/starling/tool"
)

func TestReplayCmd_NilFactory_DualModeError(t *testing.T) {
	c := starling.ReplayCommand(nil)
	c.Output = &bytes.Buffer{}
	err := c.Run([]string{"/some/db", "runid"})
	if err == nil {
		t.Fatal("expected dual-mode error")
	}
	if !strings.Contains(err.Error(), "dual-mode") {
		t.Errorf("err = %q, want dual-mode guidance", err.Error())
	}
}

func TestReplayCmd_OK(t *testing.T) {
	// Live run with a tool round-trip so replay has something real to
	// re-execute. Seeded on-disk so the command can open it.
	db, runID, tools := seedSQLiteToolRun(t)

	factory := func(ctx context.Context) (replay.Agent, error) {
		return &starling.Agent{
			// Provider is replaced by Replay; any non-nil value
			// satisfies validate().
			Provider: &cannedProvider{},
			Tools:    tools,
			Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
		}, nil
	}

	var buf bytes.Buffer
	c := starling.ReplayCommand(factory)
	c.Output = &buf
	if err := c.Run([]string{db, runID}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "OK:") {
		t.Errorf("output = %q, want OK line", buf.String())
	}
}

func TestReplayCmd_Diverged(t *testing.T) {
	db, runID, _ := seedSQLiteToolRun(t)

	// Factory returns an agent whose echo tool drifts.
	driftTool := tool.Typed("echo", "echo msg", func(_ context.Context, in echoIn) (echoOut, error) {
		return echoOut{Got: in.Msg + "-drift"}, nil
	})
	factory := func(ctx context.Context) (replay.Agent, error) {
		return &starling.Agent{
			Provider: &cannedProvider{},
			Tools:    []tool.Tool{driftTool},
			Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
		}, nil
	}
	var buf bytes.Buffer
	c := starling.ReplayCommand(factory)
	c.Output = &buf
	err := c.Run([]string{db, runID})
	if err == nil {
		t.Fatal("expected divergence error")
	}
	if !errors.Is(err, starling.ErrNonDeterminism) {
		t.Errorf("err = %v, want ErrNonDeterminism", err)
	}
	if !strings.Contains(buf.String(), "DIVERGED:") {
		t.Errorf("output = %q, want DIVERGED line", buf.String())
	}
}

func TestReplayCmd_BadArgs(t *testing.T) {
	factory := func(ctx context.Context) (replay.Agent, error) {
		return nil, errors.New("unused")
	}
	c := starling.ReplayCommand(factory)
	c.Output = &bytes.Buffer{}
	if err := c.Run([]string{filepath.Join(t.TempDir(), "x.db")}); err == nil {
		t.Fatal("expected error for missing runID")
	}
}

// seedSQLiteToolRun runs a tool-round-trip agent against a fresh
// on-disk SQLite log and returns the db path, runID, and the tool
// slice used so the replay factory can mirror the registry.
func seedSQLiteToolRun(t *testing.T) (string, string, []tool.Tool) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "runs.db")
	log, err := eventlog.NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 10, OutputTokens: 5}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
		{
			{Kind: provider.ChunkText, Text: "got ping"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 20, OutputTokens: 3}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	tools := []tool.Tool{echoTool()}
	a := &starling.Agent{
		Provider: p,
		Tools:    tools,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	res, err := a.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	return dbPath, res.RunID, tools
}
