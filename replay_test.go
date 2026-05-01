package starling_test

import (
	"context"
	"errors"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/starlingtest"
	"github.com/jerkeyray/starling/tool"
)

func TestReplay_TextOnlyRoundTrip(t *testing.T) {
	// Live run with a canned provider emitting a one-turn completion.
	p := &starlingtest.ScriptedProvider{Scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "hello there"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 5, OutputTokens: 3}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Replay against the same Agent (provider gets swapped internally).
	if err := starling.Replay(context.Background(), log, res.RunID, a); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

func TestReplay_ToolRoundTrip(t *testing.T) {
	// Two-turn run: turn 1 plans a tool call, turn 2 produces final text.
	p := &starlingtest.ScriptedProvider{Scripts: [][]provider.StreamChunk{
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
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	res, err := a.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if err := starling.Replay(context.Background(), log, res.RunID, a); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

func TestReplay_DivergentToolReturnsErrNonDeterminism(t *testing.T) {
	// Live run captures echo("ping") -> "ping".
	p := &starlingtest.ScriptedProvider{Scripts: [][]provider.StreamChunk{
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
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	res, err := a.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Replay with a divergent tool: same name, different output.
	driftTool := tool.Typed("echo", "echo msg", func(_ context.Context, in echoIn) (echoOut, error) {
		return echoOut{Got: in.Msg + "-drift"}, nil
	})
	a2 := &starling.Agent{
		Provider: p, // ignored — Replay swaps it
		Tools:    []tool.Tool{driftTool},
		Log:      log,
		Config:   a.Config,
	}
	err = starling.Replay(context.Background(), log, res.RunID, a2)
	if !errors.Is(err, starling.ErrNonDeterminism) {
		t.Fatalf("Replay err = %v, want ErrNonDeterminism", err)
	}
}

func TestReplay_RefusesOnProviderMismatch(t *testing.T) {
	p := &starlingtest.ScriptedProvider{Scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "ok"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "m1"},
	}
	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	p.Reset()
	a2 := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "m2"},
	}
	err = starling.Replay(context.Background(), log, res.RunID, a2)
	if !errors.Is(err, starling.ErrProviderModelMismatch) {
		t.Fatalf("err = %v, want ErrProviderModelMismatch", err)
	}
}

func TestReplay_WithForceProvider_BypassesCheck(t *testing.T) {
	p := &starlingtest.ScriptedProvider{Scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "ok"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{Provider: p, Log: log, Config: starling.Config{Model: "m1"}}
	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	p.Reset()
	a2 := &starling.Agent{Provider: p, Log: log, Config: starling.Config{Model: "m2"}}
	// WithForceProvider bypasses the upfront identity check. The
	// byte-for-byte replay still trips on the divergent RunStarted
	// payload (ModelID m2 vs m1) — but that surfaces as
	// ErrNonDeterminism instead of ErrProviderModelMismatch.
	err = starling.Replay(context.Background(), log, res.RunID, a2, starling.WithForceProvider())
	if errors.Is(err, starling.ErrProviderModelMismatch) {
		t.Fatalf("force did not bypass identity check: %v", err)
	}
	if !errors.Is(err, starling.ErrNonDeterminism) {
		t.Fatalf("err = %v, want ErrNonDeterminism (RunStarted payload diverges)", err)
	}
}

func TestReplay_UnknownRunReturnsError(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{
		Provider: &starlingtest.ScriptedProvider{},
		Log:      log,
		Config:   starling.Config{Model: "x"},
	}
	err := starling.Replay(context.Background(), log, "nonexistent", a)
	if err == nil {
		t.Fatal("want error on unknown run")
	}
}
