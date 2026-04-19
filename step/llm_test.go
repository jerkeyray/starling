package step_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/step"
)

// ---- fakes ---------------------------------------------------------------

type fakeStream struct {
	chunks []provider.StreamChunk
	i      int
	errAt  int // return fakeErr at Next call index N (1-based), 0 = disabled
	err    error
	closed bool
}

func (s *fakeStream) Next(ctx context.Context) (provider.StreamChunk, error) {
	if s.errAt > 0 && s.i+1 == s.errAt {
		s.i++
		return provider.StreamChunk{}, s.err
	}
	if s.i >= len(s.chunks) {
		return provider.StreamChunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

type fakeProvider struct {
	stream *fakeStream
	info   provider.Info
	openErr error
}

func (p *fakeProvider) Info() provider.Info { return p.info }

func (p *fakeProvider) Stream(ctx context.Context, req *provider.Request) (provider.EventStream, error) {
	if p.openErr != nil {
		return nil, p.openErr
	}
	return p.stream, nil
}

// ---- helpers -------------------------------------------------------------

func newLLMCtx(t *testing.T, p provider.Provider, budget step.BudgetConfig) (context.Context, eventlog.EventLog) {
	t.Helper()
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	c := step.NewContext(step.Config{
		Log:      log,
		RunID:    "run-llm-1",
		Provider: p,
		Budget:   budget,
	})
	return step.WithContext(context.Background(), c), log
}

func readAll(t *testing.T, log eventlog.EventLog) []event.Event {
	t.Helper()
	evs, err := log.Read(context.Background(), "run-llm-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return evs
}

// ---- tests ---------------------------------------------------------------

func TestLLMCall_TextOnly(t *testing.T) {
	usage := provider.UsageUpdate{InputTokens: 50, OutputTokens: 10}
	chunks := []provider.StreamChunk{
		{Kind: provider.ChunkText, Text: "hello "},
		{Kind: provider.ChunkText, Text: "world"},
		{Kind: provider.ChunkUsage, Usage: &usage},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}
	p := &fakeProvider{stream: &fakeStream{chunks: chunks}}
	ctx, log := newLLMCtx(t, p, step.BudgetConfig{})

	resp, err := step.LLMCall(ctx, &provider.Request{Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("LLMCall: %v", err)
	}
	if resp.Text != "hello world" {
		t.Fatalf("Text = %q, want %q", resp.Text, "hello world")
	}
	if resp.Usage != usage {
		t.Fatalf("Usage = %+v, want %+v", resp.Usage, usage)
	}
	if len(resp.ToolUses) != 0 {
		t.Fatalf("ToolUses len = %d, want 0", len(resp.ToolUses))
	}
	if resp.StopReason != "stop" {
		t.Fatalf("StopReason = %q", resp.StopReason)
	}

	evs := readAll(t, log)
	if len(evs) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(evs))
	}
	if evs[0].Kind != event.KindTurnStarted {
		t.Fatalf("events[0].Kind = %s, want TurnStarted", evs[0].Kind)
	}
	if evs[1].Kind != event.KindAssistantMessageCompleted {
		t.Fatalf("events[1].Kind = %s, want AssistantMessageCompleted", evs[1].Kind)
	}
	amc, err := evs[1].AsAssistantMessageCompleted()
	if err != nil {
		t.Fatalf("decode AMC: %v", err)
	}
	if amc.Text != "hello world" {
		t.Fatalf("amc.Text = %q", amc.Text)
	}
	if amc.InputTokens != 50 || amc.OutputTokens != 10 {
		t.Fatalf("tokens = (%d, %d)", amc.InputTokens, amc.OutputTokens)
	}
	if amc.CostUSD <= 0 {
		t.Fatalf("CostUSD = %v, want > 0 for gpt-4o-mini", amc.CostUSD)
	}
	if !p.stream.closed {
		t.Fatalf("stream not closed")
	}
}

func TestLLMCall_WithToolUses(t *testing.T) {
	chunks := []provider.StreamChunk{
		{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "search"}},
		{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"q":`}},
		{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `"go"}`}},
		{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
		{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
		{Kind: provider.ChunkEnd, StopReason: "tool_use"},
	}
	p := &fakeProvider{stream: &fakeStream{chunks: chunks}}
	ctx, log := newLLMCtx(t, p, step.BudgetConfig{})

	resp, err := step.LLMCall(ctx, &provider.Request{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("LLMCall: %v", err)
	}
	if len(resp.ToolUses) != 1 {
		t.Fatalf("ToolUses len = %d, want 1", len(resp.ToolUses))
	}
	tu := resp.ToolUses[0]
	if tu.CallID != "c1" || tu.Name != "search" {
		t.Fatalf("tool use = %+v", tu)
	}
	if !bytes.Equal(tu.Args, []byte(`{"q":"go"}`)) {
		t.Fatalf("Args = %s", tu.Args)
	}

	evs := readAll(t, log)
	amc, err := evs[1].AsAssistantMessageCompleted()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(amc.ToolUses) != 1 {
		t.Fatalf("amc.ToolUses len = %d", len(amc.ToolUses))
	}
	// AMC args are canonical CBOR of {"q":"go"}; decode and compare.
	var decoded map[string]any
	if err := cborenc.Unmarshal(amc.ToolUses[0].Args, &decoded); err != nil {
		t.Fatalf("decode CBOR args: %v", err)
	}
	if decoded["q"] != "go" {
		t.Fatalf("args map = %+v", decoded)
	}
}

func TestLLMCall_PreCallBudget(t *testing.T) {
	p := &fakeProvider{stream: &fakeStream{}}
	ctx, log := newLLMCtx(t, p, step.BudgetConfig{MaxInputTokens: 1})

	req := &provider.Request{
		Model:        "gpt-4o-mini",
		SystemPrompt: "this is a long system prompt that definitely exceeds one token worth of runes",
	}
	_, err := step.LLMCall(ctx, req)
	if !errors.Is(err, step.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	evs := readAll(t, log)
	if len(evs) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(evs))
	}
	if evs[0].Kind != event.KindBudgetExceeded {
		t.Fatalf("kind = %s, want BudgetExceeded", evs[0].Kind)
	}
	be, err := evs[0].AsBudgetExceeded()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if be.Limit != "input_tokens" || be.Where != "pre_call" {
		t.Fatalf("be = %+v", be)
	}
}

func TestLLMCall_MidStream_OutputTokens(t *testing.T) {
	usage := provider.UsageUpdate{InputTokens: 10, OutputTokens: 20}
	chunks := []provider.StreamChunk{
		{Kind: provider.ChunkText, Text: "partial answer"},
		{Kind: provider.ChunkUsage, Usage: &usage},
		// Provider would continue emitting; we should never get here.
		{Kind: provider.ChunkText, Text: " SHOULD-NOT-APPEAR"},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}
	p := &fakeProvider{stream: &fakeStream{chunks: chunks}}
	ctx, log := newLLMCtx(t, p, step.BudgetConfig{MaxOutputTokens: 5})

	_, err := step.LLMCall(ctx, &provider.Request{Model: "gpt-4o-mini"})
	if !errors.Is(err, step.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	evs := readAll(t, log)
	wantKinds := []event.Kind{event.KindTurnStarted, event.KindBudgetExceeded}
	if len(evs) != len(wantKinds) {
		t.Fatalf("len(events) = %d, want %d: %v", len(evs), len(wantKinds), evs)
	}
	for i, k := range wantKinds {
		if evs[i].Kind != k {
			t.Fatalf("kind[%d] = %s, want %s", i, evs[i].Kind, k)
		}
	}
	be, err := evs[1].AsBudgetExceeded()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if be.Limit != "output_tokens" {
		t.Fatalf("Limit = %q", be.Limit)
	}
	if be.Where != "mid_stream" {
		t.Fatalf("Where = %q", be.Where)
	}
	if be.Cap != 5 || be.Actual != 20 {
		t.Fatalf("cap=%v actual=%v", be.Cap, be.Actual)
	}
	if be.PartialText != "partial answer" {
		t.Fatalf("PartialText = %q", be.PartialText)
	}
	if be.PartialTokens != 20 {
		t.Fatalf("PartialTokens = %d, want 20", be.PartialTokens)
	}
	if be.TurnID == "" {
		t.Fatalf("TurnID empty")
	}
}

func TestLLMCall_MidStream_USD(t *testing.T) {
	// gpt-4o-mini: $0.15/$0.60 per Mtok. 1M out = $0.60. Cap $0.0001 trips.
	usage := provider.UsageUpdate{InputTokens: 0, OutputTokens: 1_000_000}
	chunks := []provider.StreamChunk{
		{Kind: provider.ChunkUsage, Usage: &usage},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}
	p := &fakeProvider{stream: &fakeStream{chunks: chunks}}
	ctx, log := newLLMCtx(t, p, step.BudgetConfig{MaxUSD: 0.0001})

	_, err := step.LLMCall(ctx, &provider.Request{Model: "gpt-4o-mini"})
	if !errors.Is(err, step.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	evs := readAll(t, log)
	if len(evs) != 2 || evs[1].Kind != event.KindBudgetExceeded {
		t.Fatalf("events = %v", evs)
	}
	be, _ := evs[1].AsBudgetExceeded()
	if be.Limit != "usd" {
		t.Fatalf("Limit = %q", be.Limit)
	}
	if be.Where != "mid_stream" {
		t.Fatalf("Where = %q", be.Where)
	}
	if be.Cap != 0.0001 || be.Actual <= be.Cap {
		t.Fatalf("cap=%v actual=%v", be.Cap, be.Actual)
	}
}

func TestLLMCall_MidStream_UnknownModelUSDSkipped(t *testing.T) {
	// Unknown model → CostUSD returns 0, USD check silently skipped.
	usage := provider.UsageUpdate{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	chunks := []provider.StreamChunk{
		{Kind: provider.ChunkUsage, Usage: &usage},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}
	p := &fakeProvider{stream: &fakeStream{chunks: chunks}}
	ctx, _ := newLLMCtx(t, p, step.BudgetConfig{MaxUSD: 0.0001})

	_, err := step.LLMCall(ctx, &provider.Request{Model: "mystery-model-x"})
	if err != nil {
		t.Fatalf("LLMCall: %v (expected success; unknown model skips USD check)", err)
	}
}

func TestLLMCall_StreamError(t *testing.T) {
	boom := errors.New("boom")
	chunks := []provider.StreamChunk{
		{Kind: provider.ChunkText, Text: "partial"},
	}
	p := &fakeProvider{stream: &fakeStream{chunks: chunks, errAt: 2, err: boom}}
	ctx, log := newLLMCtx(t, p, step.BudgetConfig{})

	_, err := step.LLMCall(ctx, &provider.Request{Model: "gpt-4o"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	evs := readAll(t, log)
	if len(evs) != 1 || evs[0].Kind != event.KindTurnStarted {
		t.Fatalf("events = %v, want [TurnStarted]", evs)
	}
}

func TestLLMCall_CostLookup(t *testing.T) {
	usage := provider.UsageUpdate{InputTokens: 1000, OutputTokens: 1000}
	chunks := []provider.StreamChunk{
		{Kind: provider.ChunkUsage, Usage: &usage},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}

	// unknown model
	p1 := &fakeProvider{stream: &fakeStream{chunks: chunks}}
	ctx1, _ := newLLMCtx(t, p1, step.BudgetConfig{})
	r1, err := step.LLMCall(ctx1, &provider.Request{Model: "mystery-model-z"})
	if err != nil {
		t.Fatalf("LLMCall: %v", err)
	}
	if r1.CostUSD != 0 {
		t.Fatalf("unknown model CostUSD = %v, want 0", r1.CostUSD)
	}

	// known model
	p2 := &fakeProvider{stream: &fakeStream{chunks: chunks}}
	ctx2, _ := newLLMCtx(t, p2, step.BudgetConfig{})
	r2, err := step.LLMCall(ctx2, &provider.Request{Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("LLMCall: %v", err)
	}
	if r2.CostUSD <= 0 {
		t.Fatalf("gpt-4o-mini CostUSD = %v, want > 0", r2.CostUSD)
	}
}

func TestLLMCall_Deterministic(t *testing.T) {
	mkChunks := func() []provider.StreamChunk {
		return []provider.StreamChunk{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "search"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"q":"go","n":42}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkText, Text: "ok"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 10, OutputTokens: 5}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		}
	}

	run := func() []byte {
		p := &fakeProvider{stream: &fakeStream{chunks: mkChunks()}}
		ctx, log := newLLMCtx(t, p, step.BudgetConfig{})
		_, err := step.LLMCall(ctx, &provider.Request{Model: "gpt-4o"})
		if err != nil {
			t.Fatalf("LLMCall: %v", err)
		}
		evs := readAll(t, log)
		amc, err := evs[1].AsAssistantMessageCompleted()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		// TurnID is a random ULID per run — zero it so we can compare
		// the deterministic fields of the payload.
		amc.TurnID = ""
		b, err := event.EncodePayload(amc)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		return b
	}

	a, b := run(), run()
	if !bytes.Equal(a, b) {
		t.Fatalf("AMC payloads (TurnID zeroed) differ across runs:\n a: %x\n b: %x", a, b)
	}
}
