package starling_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

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

	// ToolCallScheduled.TurnID must equal the preceding TurnStarted.TurnID
	// so replay and external consumers can correlate without a seq walk.
	ts, err := evs[1].AsTurnStarted()
	if err != nil {
		t.Fatalf("decode TurnStarted: %v", err)
	}
	tcs, err := evs[3].AsToolCallScheduled()
	if err != nil {
		t.Fatalf("decode Scheduled: %v", err)
	}
	if tcs.TurnID == "" || tcs.TurnID != ts.TurnID {
		t.Fatalf("TurnID mismatch: scheduled=%q turn=%q", tcs.TurnID, ts.TurnID)
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

// TestAgent_ParallelToolDispatch_ReplayMatches exercises the two-tool
// fan-out path: turn 1 plans two tools at once, turn 2 produces final
// text. The live run races the two tools; replay dispatches them in
// recorded completion order and must reproduce the log byte-for-byte.
func TestAgent_ParallelToolDispatch_ReplayMatches(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"a"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c2", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c2", ArgsDelta: `{"msg":"b"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c2"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 10, OutputTokens: 5}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
		{
			{Kind: provider.ChunkText, Text: "done"},
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
	res, err := a.Run(context.Background(), "do both")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TerminalKind != event.KindRunCompleted {
		t.Fatalf("TerminalKind = %s, want RunCompleted", res.TerminalKind)
	}

	// Both tools' Scheduled events must appear in provider-plan order
	// (c1, c2), regardless of which Completed landed first.
	evs, err := log.Read(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var sched []string
	for _, ev := range evs {
		if ev.Kind == event.KindToolCallScheduled {
			s, _ := ev.AsToolCallScheduled()
			sched = append(sched, s.CallID)
		}
	}
	if len(sched) != 2 || sched[0] != "c1" || sched[1] != "c2" {
		t.Fatalf("Scheduled order = %v, want [c1 c2]", sched)
	}

	// Replay should reproduce the run byte-for-byte even though the
	// live run raced.
	if err := starling.Replay(context.Background(), log, res.RunID, a); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

// TestAgent_AnthropicThinking_ReplayMatches exercises the
// extended-thinking flow a real Anthropic response produces: streaming
// reasoning text deltas, a terminal signature chunk, then the final
// assistant text. Live run emits one ReasoningEmitted with both
// Content and Signature populated, and Replay reproduces the log
// byte-for-byte.
func TestAgent_AnthropicThinking_ReplayMatches(t *testing.T) {
	sig := []byte("sig-bytes-v1")
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			// Two thinking deltas accumulate.
			{Kind: provider.ChunkReasoning, Text: "let me think "},
			{Kind: provider.ChunkReasoning, Text: "about this..."},
			// Terminal signature flushes the buffered reasoning.
			{Kind: provider.ChunkReasoning, Signature: sig},
			// Final assistant text.
			{Kind: provider.ChunkText, Text: "the answer is 42"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 8, OutputTokens: 6}},
			{Kind: provider.ChunkEnd, StopReason: "end_turn"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "claude-sonnet-4-6", MaxTurns: 2},
	}
	res, err := a.Run(context.Background(), "think hard")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TerminalKind != event.KindRunCompleted {
		t.Fatalf("TerminalKind = %s", res.TerminalKind)
	}
	if res.FinalText != "the answer is 42" {
		t.Fatalf("FinalText = %q", res.FinalText)
	}

	evs, err := log.Read(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// Exactly one ReasoningEmitted; Content = concatenated deltas;
	// Signature = captured sig bytes.
	var reCount int
	for _, ev := range evs {
		if ev.Kind != event.KindReasoningEmitted {
			continue
		}
		reCount++
		re, err := ev.AsReasoningEmitted()
		if err != nil {
			t.Fatalf("decode ReasoningEmitted: %v", err)
		}
		if re.Content != "let me think about this..." {
			t.Fatalf("ReasoningEmitted.Content = %q", re.Content)
		}
		if string(re.Signature) != string(sig) {
			t.Fatalf("ReasoningEmitted.Signature = %q, want %q", re.Signature, sig)
		}
		if re.Redacted {
			t.Fatalf("ReasoningEmitted.Redacted = true, want false")
		}
	}
	if reCount != 1 {
		t.Fatalf("ReasoningEmitted count = %d, want 1", reCount)
	}

	// Reset the provider and replay: must be byte-identical.
	p.i = 0
	if err := starling.Replay(context.Background(), log, res.RunID, a); err != nil {
		t.Fatalf("Replay: %v", err)
	}
}

// TestAgent_Budget_OutputTokens_MidStream covers the step-layer
// mid-stream trip: a usage chunk reporting 20 output tokens against a
// cap of 5 emits BudgetExceeded{mid_stream,output_tokens} and
// terminates the run as RunFailed{ErrorType:"budget"}. Replay of the
// resulting log must be byte-identical.
func TestAgent_Budget_OutputTokens_MidStream(t *testing.T) {
	usage := provider.UsageUpdate{InputTokens: 5, OutputTokens: 20}
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "partial"},
			{Kind: provider.ChunkUsage, Usage: &usage},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
		Budget:   &starling.Budget{MaxOutputTokens: 5},
	}
	res, err := a.Run(context.Background(), "go")
	if err == nil {
		t.Fatalf("err = nil, want a budget error")
	}
	if res.TerminalKind != event.KindRunFailed {
		t.Fatalf("TerminalKind = %s", res.TerminalKind)
	}

	evs, _ := log.Read(context.Background(), res.RunID)
	want := []event.Kind{
		event.KindRunStarted,
		event.KindTurnStarted,
		event.KindBudgetExceeded,
		event.KindRunFailed,
	}
	if got := kindsOf(evs); !kindsEq(got, want) {
		t.Fatalf("kinds = %v\n want %v", got, want)
	}
	be, _ := evs[2].AsBudgetExceeded()
	if be.Limit != "output_tokens" || be.Where != "mid_stream" {
		t.Fatalf("be = %+v", be)
	}
	rf, _ := evs[3].AsRunFailed()
	if rf.ErrorType != "budget" {
		t.Fatalf("RunFailed.ErrorType = %q", rf.ErrorType)
	}
	// Replay of mid-stream budget trips is deferred: the replay
	// provider reconstructs streams from AssistantMessageCompleted,
	// which a tripped turn never emits. Tracked separately.
}

// blockingStream blocks on Next until ctx cancels. Used for the
// wall-clock budget test where the provider is "stuck" and the run
// must unblock via the deadline.
type blockingStream struct{}

func (blockingStream) Next(ctx context.Context) (provider.StreamChunk, error) {
	<-ctx.Done()
	return provider.StreamChunk{}, ctx.Err()
}
func (blockingStream) Close() error { return nil }

type blockingProvider struct{}

func (blockingProvider) Info() provider.Info { return provider.Info{ID: "blocking", APIVersion: "v0"} }
func (blockingProvider) Stream(_ context.Context, _ *provider.Request) (provider.EventStream, error) {
	return blockingStream{}, nil
}

// TestAgent_Budget_WallClock verifies the agent-level deadline path:
// a provider that blocks forever should unblock within the wall-clock
// budget and terminate as RunFailed{budget} with a preceding
// BudgetExceeded{wall_clock} event.
func TestAgent_Budget_WallClock(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: blockingProvider{},
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
		Budget:   &starling.Budget{MaxWallClock: 50 * time.Millisecond},
	}
	start := time.Now()
	res, err := a.Run(context.Background(), "wait")
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed = %v, want < 500ms (deadline should preempt the blocking stream)", elapsed)
	}
	if res.TerminalKind != event.KindRunFailed {
		t.Fatalf("TerminalKind = %s, want RunFailed", res.TerminalKind)
	}

	evs, _ := log.Read(context.Background(), res.RunID)
	// Find the BudgetExceeded immediately before RunFailed.
	if len(evs) < 2 {
		t.Fatalf("events too short: %v", kindsOf(evs))
	}
	last := evs[len(evs)-1]
	penult := evs[len(evs)-2]
	if last.Kind != event.KindRunFailed {
		t.Fatalf("final kind = %s, want RunFailed", last.Kind)
	}
	if penult.Kind != event.KindBudgetExceeded {
		t.Fatalf("penult kind = %s, want BudgetExceeded", penult.Kind)
	}
	be, _ := penult.AsBudgetExceeded()
	if be.Limit != "wall_clock" {
		t.Fatalf("Limit = %q, want wall_clock", be.Limit)
	}
	rf, _ := last.AsRunFailed()
	if rf.ErrorType != "budget" {
		t.Fatalf("RunFailed.ErrorType = %q, want budget", rf.ErrorType)
	}
}

// ---- namespace -----------------------------------------------------------

func TestAgent_Namespace_PrefixesRunID(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{{
		{Kind: provider.ChunkText, Text: "ok"},
		{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider:  p,
		Log:       log,
		Namespace: "tenantA",
		Config:    starling.Config{Model: "m", MaxTurns: 2},
	}
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "tenantA/"; len(res.RunID) < len(want) || res.RunID[:len(want)] != want {
		t.Fatalf("RunID = %q, want prefix %q", res.RunID, want)
	}
	// Log is keyed on the prefixed RunID.
	evs, _ := log.Read(context.Background(), res.RunID)
	if len(evs) == 0 {
		t.Fatalf("no events under prefixed RunID")
	}
	if evs[0].RunID != res.RunID {
		t.Fatalf("event RunID = %q, want %q", evs[0].RunID, res.RunID)
	}
}

func TestAgent_Namespace_RejectsSlash(t *testing.T) {
	p := &cannedProvider{}
	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{
		Provider:  p,
		Log:       log,
		Namespace: "a/b",
		Config:    starling.Config{Model: "m"},
	}
	if _, err := a.Run(context.Background(), "x"); err == nil {
		t.Fatal("want error for namespace containing '/'")
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
