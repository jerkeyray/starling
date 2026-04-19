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

func TestCallTool_PanicsWithoutContext(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic")
		}
	}()
	_, _ = step.CallTool(context.Background(), step.ToolCall{Name: "x"})
}
