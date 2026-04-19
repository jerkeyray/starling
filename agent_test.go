package starling_test

import (
	"context"
	"errors"
	"io"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/tool"
)

// ---- canned provider -----------------------------------------------------

// cannedProvider returns a pre-built slice of StreamChunk sequences,
// one per Stream call. Deterministic and has no network.
type cannedProvider struct {
	scripts [][]provider.StreamChunk
	i       int
}

func (p *cannedProvider) Info() provider.Info {
	return provider.Info{ID: "canned", APIVersion: "v0"}
}

func (p *cannedProvider) Stream(_ context.Context, _ *provider.Request) (provider.EventStream, error) {
	if p.i >= len(p.scripts) {
		return nil, errors.New("cannedProvider: exhausted")
	}
	s := &cannedStream{chunks: p.scripts[p.i]}
	p.i++
	return s, nil
}

type cannedStream struct {
	chunks []provider.StreamChunk
	j      int
}

func (s *cannedStream) Next(context.Context) (provider.StreamChunk, error) {
	if s.j >= len(s.chunks) {
		return provider.StreamChunk{}, io.EOF
	}
	c := s.chunks[s.j]
	s.j++
	return c, nil
}

func (s *cannedStream) Close() error { return nil }

// ---- tools ---------------------------------------------------------------

type echoIn struct {
	Msg string `json:"msg"`
}
type echoOut struct {
	Got string `json:"got"`
}

func echoTool() tool.Tool {
	return tool.Typed("echo", "echo msg", func(_ context.Context, in echoIn) (echoOut, error) {
		return echoOut{Got: in.Msg}, nil
	})
}

// ---- helpers -------------------------------------------------------------

func kindsOf(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i := range evs {
		out[i] = evs[i].Kind
	}
	return out
}

// ---- tests ---------------------------------------------------------------

func TestAgent_TextOnly_OneTurnCompletes(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
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
	if res.TerminalKind != event.KindRunCompleted {
		t.Fatalf("TerminalKind = %s, want RunCompleted", res.TerminalKind)
	}
	if res.FinalText != "hello there" {
		t.Fatalf("FinalText = %q", res.FinalText)
	}

	evs, _ := log.Read(context.Background(), res.RunID)
	want := []event.Kind{
		event.KindRunStarted,
		event.KindTurnStarted,
		event.KindAssistantMessageCompleted,
		event.KindRunCompleted,
	}
	if got := kindsOf(evs); !kindsEq(got, want) {
		t.Fatalf("kinds = %v\n want %v", got, want)
	}
	if err := eventlog.Validate(evs); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestAgent_ToolRoundTrip(t *testing.T) {
	// Turn 1: plan a tool call. Turn 2: after tool result, produce final text.
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
	if res.TerminalKind != event.KindRunCompleted {
		t.Fatalf("TerminalKind = %s", res.TerminalKind)
	}
	if res.FinalText != "got ping" {
		t.Fatalf("FinalText = %q", res.FinalText)
	}
	if res.TurnCount != 2 || res.ToolCallCount != 1 {
		t.Fatalf("counts = turns=%d tools=%d, want 2/1", res.TurnCount, res.ToolCallCount)
	}

	evs, _ := log.Read(context.Background(), res.RunID)
	want := []event.Kind{
		event.KindRunStarted,
		event.KindTurnStarted,
		event.KindAssistantMessageCompleted,
		event.KindToolCallScheduled,
		event.KindToolCallCompleted,
		event.KindTurnStarted,
		event.KindAssistantMessageCompleted,
		event.KindRunCompleted,
	}
	if got := kindsOf(evs); !kindsEq(got, want) {
		t.Fatalf("kinds = %v\n want %v", got, want)
	}

	// Verify the echo tool's result made it into the log.
	tcc, err := evs[4].AsToolCallCompleted()
	if err != nil {
		t.Fatalf("decode Completed: %v", err)
	}
	if tcc.CallID != "c1" {
		t.Fatalf("CallID = %q", tcc.CallID)
	}
}

func TestAgent_MerkleRootStable(t *testing.T) {
	// Two identical runs produce the same Merkle root (modulo TurnIDs,
	// which are random ULIDs). We can't compare roots across runs
	// because TurnID is in the TurnStarted payload; we just check that
	// the root is 32 bytes and non-zero.
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "ok"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{Provider: p, Log: log, Config: starling.Config{Model: "gpt-4o"}}
	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.MerkleRoot) != 32 {
		t.Fatalf("MerkleRoot len = %d, want 32", len(res.MerkleRoot))
	}
	var zero [32]byte
	if string(res.MerkleRoot) == string(zero[:]) {
		t.Fatalf("MerkleRoot is all zeros")
	}
}

func TestAgent_MaxTurnsExceeded(t *testing.T) {
	// Every turn plans a tool call, so the loop never terminates
	// naturally; MaxTurns=2 trips after two assistant turns.
	mkToolUseTurn := func() []provider.StreamChunk {
		return []provider.StreamChunk{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c", ArgsDelta: `{"msg":"x"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		}
	}
	p := &cannedProvider{scripts: [][]provider.StreamChunk{mkToolUseTurn(), mkToolUseTurn()}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 2},
	}
	res, err := a.Run(context.Background(), "spin")
	if !errors.Is(err, starling.ErrMaxTurnsExceeded) {
		t.Fatalf("err = %v, want ErrMaxTurnsExceeded", err)
	}
	if res == nil || res.RunID == "" {
		t.Fatalf("Run returned nil result despite terminal emission")
	}
	evs, _ := log.Read(context.Background(), res.RunID)
	last := evs[len(evs)-1]
	if last.Kind != event.KindRunFailed {
		t.Fatalf("last kind = %s, want RunFailed", last.Kind)
	}
	rf, _ := last.AsRunFailed()
	if rf.ErrorType != "max_turns" {
		t.Fatalf("ErrorType = %q, want max_turns", rf.ErrorType)
	}
}

func TestAgent_ValidationErrors(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()

	_, err := (&starling.Agent{Log: log, Config: starling.Config{Model: "m"}}).Run(context.Background(), "x")
	if err == nil {
		t.Fatalf("want error for missing Provider")
	}

	p := &cannedProvider{}
	_, err = (&starling.Agent{Provider: p, Config: starling.Config{Model: "m"}}).Run(context.Background(), "x")
	if err == nil {
		t.Fatalf("want error for missing Log")
	}

	_, err = (&starling.Agent{Provider: p, Log: log}).Run(context.Background(), "x")
	if err == nil {
		t.Fatalf("want error for empty Model")
	}

	_, err = (&starling.Agent{
		Provider: p, Log: log,
		Config: starling.Config{Model: "m"},
		Tools:  []tool.Tool{echoTool(), echoTool()},
	}).Run(context.Background(), "x")
	if err == nil {
		t.Fatalf("want error for duplicate tool names")
	}
}

func TestAgent_Cancelled(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "hi"},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so react() bails on first ctx.Err check

	a := &starling.Agent{Provider: p, Log: log, Config: starling.Config{Model: "m"}}
	res, err := a.Run(ctx, "go")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res == nil || res.RunID == "" {
		t.Fatalf("Run returned nil result despite terminal emission")
	}
	// Still must see RunStarted + RunCancelled.
	evs, _ := log.Read(context.Background(), res.RunID)
	if evs[0].Kind != event.KindRunStarted {
		t.Fatalf("events[0] = %s", evs[0].Kind)
	}
	if evs[len(evs)-1].Kind != event.KindRunCancelled {
		t.Fatalf("last = %s, want RunCancelled", evs[len(evs)-1].Kind)
	}
}

// TestAgent_ToolRegistryHashStableUnderReorder pins the fix for a
// spec-drift bug: emitRunStarted must hash tools in alphabetical order
// so the same set of tools in different declaration order produces the
// same ToolRegistryHash. step.Registry.Names() is alphabetical by
// contract; the event emission path must match.
func TestAgent_ToolRegistryHashStableUnderReorder(t *testing.T) {
	mkScript := func() [][]provider.StreamChunk {
		return [][]provider.StreamChunk{{
			{Kind: provider.ChunkText, Text: "ok"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		}}
	}
	zebra := tool.Typed("zebra", "", func(_ context.Context, _ struct{}) (struct{}, error) { return struct{}{}, nil })
	alpha := tool.Typed("alpha", "", func(_ context.Context, _ struct{}) (struct{}, error) { return struct{}{}, nil })

	runOnce := func(tools []tool.Tool) []byte {
		log := eventlog.NewInMemory()
		defer log.Close()
		a := &starling.Agent{
			Provider: &cannedProvider{scripts: mkScript()},
			Tools:    tools,
			Log:      log,
			Config:   starling.Config{Model: "m"},
		}
		res, err := a.Run(context.Background(), "go")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		evs, _ := log.Read(context.Background(), res.RunID)
		rs, err := evs[0].AsRunStarted()
		if err != nil {
			t.Fatalf("AsRunStarted: %v", err)
		}
		return rs.ToolRegistryHash
	}

	h1 := runOnce([]tool.Tool{alpha, zebra})
	h2 := runOnce([]tool.Tool{zebra, alpha})
	if string(h1) != string(h2) {
		t.Fatalf("ToolRegistryHash differs under reorder:\n  [alpha,zebra]=%x\n  [zebra,alpha]=%x", h1, h2)
	}
}

// ---- tiny helpers --------------------------------------------------------

func kindsEq(a, b []event.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

