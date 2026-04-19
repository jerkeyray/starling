package step_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/step"
	"github.com/jerkeyray/starling/tool"
)

// ---- helpers -------------------------------------------------------------

type echoIn struct {
	Msg string `json:"msg"`
}

type echoOut struct {
	Got string `json:"got"`
}

func echoTool() tool.Tool {
	return tool.Typed("echo", "echoes msg", func(_ context.Context, in echoIn) (echoOut, error) {
		return echoOut{Got: in.Msg}, nil
	})
}

func errTool(err error) tool.Tool {
	return tool.Typed("boom", "always errors", func(_ context.Context, _ struct{}) (struct{}, error) {
		return struct{}{}, err
	})
}

// blockingTool returns a Tool implementation (not via Typed) that
// selects on ctx.Done and a wake channel — so we can force a cancelled
// outcome deterministically.
type blockingTool struct {
	wake chan struct{}
}

func (b *blockingTool) Name() string              { return "blocker" }
func (b *blockingTool) Description() string       { return "blocks until ctx cancelled" }
func (b *blockingTool) Schema() json.RawMessage   { return json.RawMessage(`{"type":"object"}`) }
func (b *blockingTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.wake:
		return json.RawMessage(`null`), nil
	}
}

// panickyTool's Execute method panics directly (Typed recovers, so we
// bypass it to exercise step's own recover).
type panickyTool struct{}

func (panickyTool) Name() string              { return "panicky" }
func (panickyTool) Description() string       { return "panics" }
func (panickyTool) Schema() json.RawMessage   { return json.RawMessage(`{"type":"object"}`) }
func (panickyTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	panic("kaboom")
}

func newToolsCtx(t *testing.T, reg *step.Registry) (context.Context, eventlog.EventLog) {
	t.Helper()
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	c := step.NewContext(step.Config{
		Log:   log,
		RunID: "run-tool-1",
		Tools: reg,
	})
	return step.WithContext(context.Background(), c), log
}

func readAllTools(t *testing.T, log eventlog.EventLog) []event.Event {
	t.Helper()
	evs, err := log.Read(context.Background(), "run-tool-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return evs
}

// ---- tests ---------------------------------------------------------------

func TestCallTool_Success(t *testing.T) {
	reg := step.NewRegistry(echoTool())
	ctx, log := newToolsCtx(t, reg)

	args := json.RawMessage(`{"msg":"hi"}`)
	out, err := step.CallTool(ctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "echo", Args: args,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var decoded echoOut
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if decoded.Got != "hi" {
		t.Fatalf("got = %q", decoded.Got)
	}

	evs := readAllTools(t, log)
	if len(evs) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(evs))
	}
	sch, err := evs[0].AsToolCallScheduled()
	if err != nil {
		t.Fatalf("decode Scheduled: %v", err)
	}
	if sch.CallID != "c1" || sch.TurnID != "t1" || sch.ToolName != "echo" {
		t.Fatalf("Scheduled = %+v", sch)
	}
	comp, err := evs[1].AsToolCallCompleted()
	if err != nil {
		t.Fatalf("decode Completed: %v", err)
	}
	if comp.CallID != "c1" {
		t.Fatalf("Completed.CallID = %q", comp.CallID)
	}
}

func TestCallTool_MintsCallIDIfEmpty(t *testing.T) {
	reg := step.NewRegistry(echoTool())
	ctx, log := newToolsCtx(t, reg)

	_, err := step.CallTool(ctx, step.ToolCall{
		TurnID: "t1", Name: "echo", Args: json.RawMessage(`{"msg":"x"}`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	evs := readAllTools(t, log)
	sch, _ := evs[0].AsToolCallScheduled()
	comp, _ := evs[1].AsToolCallCompleted()
	if len(sch.CallID) != 26 {
		t.Fatalf("minted CallID len = %d, want 26 (ULID): %q", len(sch.CallID), sch.CallID)
	}
	if sch.CallID != comp.CallID {
		t.Fatalf("CallID mismatch: sched=%q comp=%q", sch.CallID, comp.CallID)
	}
}

func TestCallTool_ToolError(t *testing.T) {
	sentinel := errors.New("bad input")
	reg := step.NewRegistry(errTool(sentinel))
	ctx, log := newToolsCtx(t, reg)

	_, err := step.CallTool(ctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "boom",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	evs := readAllTools(t, log)
	if evs[1].Kind != event.KindToolCallFailed {
		t.Fatalf("events[1].Kind = %s", evs[1].Kind)
	}
	f, _ := evs[1].AsToolCallFailed()
	if f.ErrorType != "tool" {
		t.Fatalf("ErrorType = %q, want tool", f.ErrorType)
	}
}

func TestCallTool_Panic(t *testing.T) {
	reg := step.NewRegistry(panickyTool{})
	ctx, log := newToolsCtx(t, reg)

	_, err := step.CallTool(ctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "panicky",
	})
	if !errors.Is(err, tool.ErrPanicked) {
		t.Fatalf("err = %v, want wraps tool.ErrPanicked", err)
	}
	evs := readAllTools(t, log)
	f, _ := evs[1].AsToolCallFailed()
	if f.ErrorType != "panic" {
		t.Fatalf("ErrorType = %q, want panic", f.ErrorType)
	}
}

func TestCallTool_Cancelled(t *testing.T) {
	bt := &blockingTool{wake: make(chan struct{})}
	reg := step.NewRegistry(bt)
	ctx, log := newToolsCtx(t, reg)

	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := step.CallTool(cctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "blocker",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	evs := readAllTools(t, log)
	f, _ := evs[1].AsToolCallFailed()
	if f.ErrorType != "cancelled" {
		t.Fatalf("ErrorType = %q, want cancelled", f.ErrorType)
	}
}

func TestCallTool_NotFound(t *testing.T) {
	reg := step.NewRegistry(echoTool())
	ctx, log := newToolsCtx(t, reg)

	_, err := step.CallTool(ctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "nonexistent",
	})
	if !errors.Is(err, step.ErrToolNotFound) {
		t.Fatalf("err = %v, want ErrToolNotFound", err)
	}
	evs := readAllTools(t, log)
	if len(evs) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(evs))
	}
	f, _ := evs[1].AsToolCallFailed()
	if f.ErrorType != "tool" {
		t.Fatalf("ErrorType = %q, want tool", f.ErrorType)
	}
}

// slowTool sleeps for d then echoes msg. Used by the CallTools
// latency test to prove fan-out actually overlaps execution.
func slowTool(name string, d time.Duration) tool.Tool {
	return tool.Typed(name, "slow echo", func(ctx context.Context, in echoIn) (echoOut, error) {
		select {
		case <-ctx.Done():
			return echoOut{}, ctx.Err()
		case <-time.After(d):
			return echoOut{Got: in.Msg}, nil
		}
	})
}

func TestCallTools_ParallelLatency(t *testing.T) {
	const each = 150 * time.Millisecond
	reg := step.NewRegistry(slowTool("s1", each), slowTool("s2", each), slowTool("s3", each))
	ctx, log := newToolsCtx(t, reg)

	calls := []step.ToolCall{
		{CallID: "c1", TurnID: "t1", Name: "s1", Args: json.RawMessage(`{"msg":"a"}`)},
		{CallID: "c2", TurnID: "t1", Name: "s2", Args: json.RawMessage(`{"msg":"b"}`)},
		{CallID: "c3", TurnID: "t1", Name: "s3", Args: json.RawMessage(`{"msg":"c"}`)},
	}
	start := time.Now()
	results, err := step.CallTools(ctx, calls)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CallTools: %v", err)
	}
	if elapsed > each*2 {
		t.Fatalf("elapsed = %v, want ~%v (fan-out did not overlap)", elapsed, each)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d", len(results))
	}
	// Results preserve input order even if completions raced.
	for i, res := range results {
		if res.Err != nil {
			t.Fatalf("results[%d].Err = %v", i, res.Err)
		}
		if res.CallID != calls[i].CallID {
			t.Fatalf("results[%d].CallID = %q, want %q", i, res.CallID, calls[i].CallID)
		}
	}

	evs := readAllTools(t, log)
	// Three Scheduled events must be in input order, regardless of
	// completion race.
	var scheduledIDs []string
	for _, ev := range evs {
		if ev.Kind == event.KindToolCallScheduled {
			s, _ := ev.AsToolCallScheduled()
			scheduledIDs = append(scheduledIDs, s.CallID)
		}
	}
	want := []string{"c1", "c2", "c3"}
	for i := range want {
		if scheduledIDs[i] != want[i] {
			t.Fatalf("scheduled order = %v, want %v", scheduledIDs, want)
		}
	}
	// All three Scheduled events land before any Completed (input-
	// order contiguity invariant).
	sawCompleted := false
	for _, ev := range evs {
		switch ev.Kind {
		case event.KindToolCallCompleted:
			sawCompleted = true
		case event.KindToolCallScheduled:
			if sawCompleted {
				t.Fatalf("Scheduled event at seq=%d landed after a Completed event", ev.Seq)
			}
		}
	}
}

func TestCallTools_SemaphoreCapOne(t *testing.T) {
	// With MaxParallelTools=1, two 100ms tools must take ~200ms.
	const each = 100 * time.Millisecond
	reg := step.NewRegistry(slowTool("s1", each), slowTool("s2", each))
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	c := step.NewContext(step.Config{
		Log:              log,
		RunID:            "run-tool-1",
		Tools:            reg,
		MaxParallelTools: 1,
	})
	ctx := step.WithContext(context.Background(), c)

	start := time.Now()
	_, err := step.CallTools(ctx, []step.ToolCall{
		{CallID: "c1", Name: "s1", Args: json.RawMessage(`{"msg":"a"}`)},
		{CallID: "c2", Name: "s2", Args: json.RawMessage(`{"msg":"b"}`)},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CallTools: %v", err)
	}
	if elapsed < each*2-20*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= ~%v (cap=1 did not serialize)", elapsed, each*2)
	}
}

func TestCallTools_IndividualFailureDoesNotKillBatch(t *testing.T) {
	sentinel := errors.New("bad input")
	reg := step.NewRegistry(echoTool(), errTool(sentinel))
	ctx, log := newToolsCtx(t, reg)

	results, err := step.CallTools(ctx, []step.ToolCall{
		{CallID: "c1", Name: "echo", Args: json.RawMessage(`{"msg":"ok"}`)},
		{CallID: "c2", Name: "boom"},
	})
	if err != nil {
		t.Fatalf("CallTools: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", results[0].Err)
	}
	if !errors.Is(results[1].Err, sentinel) {
		t.Fatalf("results[1].Err = %v, want sentinel", results[1].Err)
	}
	// Log contains one Completed and one Failed — proves the good
	// tool still emitted even though its sibling failed.
	evs := readAllTools(t, log)
	var completed, failed int
	for _, ev := range evs {
		switch ev.Kind {
		case event.KindToolCallCompleted:
			completed++
		case event.KindToolCallFailed:
			failed++
		}
	}
	if completed != 1 || failed != 1 {
		t.Fatalf("completed=%d failed=%d, want 1 and 1", completed, failed)
	}
}

func TestCallTools_EmptyBatchNoop(t *testing.T) {
	ctx, log := newToolsCtx(t, step.NewRegistry(echoTool()))
	results, err := step.CallTools(ctx, nil)
	if err != nil {
		t.Fatalf("CallTools: %v", err)
	}
	if results != nil {
		t.Fatalf("results = %v, want nil", results)
	}
	if evs := readAllTools(t, log); len(evs) != 0 {
		t.Fatalf("empty batch emitted %d events", len(evs))
	}
}

func TestCallTool_PanicsWithoutContext(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic")
		}
	}()
	_, _ = step.CallTool(context.Background(), step.ToolCall{Name: "x"})
}
