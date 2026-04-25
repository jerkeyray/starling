package step_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
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

func (b *blockingTool) Name() string            { return "blocker" }
func (b *blockingTool) Description() string     { return "blocks until ctx cancelled" }
func (b *blockingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
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

func (panickyTool) Name() string            { return "panicky" }
func (panickyTool) Description() string     { return "panics" }
func (panickyTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (panickyTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	panic("kaboom")
}

func newToolsCtx(t *testing.T, reg *step.Registry) (context.Context, eventlog.EventLog) {
	t.Helper()
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	c := step.MustNewContext(step.Config{
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
	c := step.MustNewContext(step.Config{
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

// flakyTool returns a Tool that returns a transient error the first
// failN calls, then succeeds. Safe for concurrent use — attempts are
// driven through an atomic counter.
type flakyTool struct {
	name  string
	failN int32
	count int32
}

func (f *flakyTool) Name() string            { return f.name }
func (f *flakyTool) Description() string     { return "flaky" }
func (f *flakyTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *flakyTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	n := atomic.AddInt32(&f.count, 1)
	if n <= f.failN {
		return nil, fmt.Errorf("attempt %d flake: %w", n, tool.ErrTransient)
	}
	return json.RawMessage(fmt.Sprintf(`{"attempt":%d}`, n)), nil
}

// noBackoff returns 0 so retry tests don't sleep.
func noBackoff(int) time.Duration { return 0 }

// tinyBackoff returns a small sleep used by the ctx-cancel test.
func tinyBackoff(int) time.Duration { return 50 * time.Millisecond }

func TestCallTool_RetrySucceedsOnThirdAttempt(t *testing.T) {
	ft := &flakyTool{name: "flaky", failN: 2}
	reg := step.NewRegistry(ft)
	ctx, log := newToolsCtx(t, reg)

	out, err := step.CallTool(ctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "flaky",
		Idempotent: true, MaxAttempts: 3, Backoff: noBackoff,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := string(out); got != `{"attempt":3}` {
		t.Fatalf("out = %s", got)
	}

	evs := readAllTools(t, log)
	wantKinds := []event.Kind{
		event.KindToolCallScheduled, event.KindToolCallFailed,
		event.KindToolCallScheduled, event.KindToolCallFailed,
		event.KindToolCallScheduled, event.KindToolCallCompleted,
	}
	if len(evs) != len(wantKinds) {
		t.Fatalf("len(events) = %d, want %d", len(evs), len(wantKinds))
	}
	for i, k := range wantKinds {
		if evs[i].Kind != k {
			t.Fatalf("events[%d].Kind = %s, want %s", i, evs[i].Kind, k)
		}
	}
	// Attempt numbers increment 1,1,2,2,3,3.
	wantAttempt := []uint32{1, 1, 2, 2, 3, 3}
	for i, ev := range evs {
		var got uint32
		switch ev.Kind {
		case event.KindToolCallScheduled:
			s, _ := ev.AsToolCallScheduled()
			got = s.Attempt
		case event.KindToolCallCompleted:
			c, _ := ev.AsToolCallCompleted()
			got = c.Attempt
		case event.KindToolCallFailed:
			f, _ := ev.AsToolCallFailed()
			got = f.Attempt
		}
		if got != wantAttempt[i] {
			t.Fatalf("events[%d].Attempt = %d, want %d", i, got, wantAttempt[i])
		}
	}
}

func TestCallTool_NonIdempotentIgnoresMaxAttempts(t *testing.T) {
	ft := &flakyTool{name: "flaky", failN: 5}
	reg := step.NewRegistry(ft)
	ctx, log := newToolsCtx(t, reg)

	_, err := step.CallTool(ctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "flaky",
		Idempotent: false, MaxAttempts: 3, Backoff: noBackoff,
	})
	if !errors.Is(err, tool.ErrTransient) {
		t.Fatalf("err = %v, want wraps ErrTransient", err)
	}
	if got := atomic.LoadInt32(&ft.count); got != 1 {
		t.Fatalf("executions = %d, want 1 (non-idempotent must not retry)", got)
	}
	evs := readAllTools(t, log)
	if len(evs) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(evs))
	}
	if evs[1].Kind != event.KindToolCallFailed {
		t.Fatalf("events[1].Kind = %s", evs[1].Kind)
	}
}

func TestCallTool_NonTransientDoesNotRetry(t *testing.T) {
	// errTool returns a plain error (no ErrTransient wrap) — retry must
	// bail on the first attempt even with Idempotent + MaxAttempts set.
	reg := step.NewRegistry(errTool(errors.New("bad input")))
	ctx, log := newToolsCtx(t, reg)

	_, err := step.CallTool(ctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "boom",
		Idempotent: true, MaxAttempts: 5, Backoff: noBackoff,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	evs := readAllTools(t, log)
	if len(evs) != 2 {
		t.Fatalf("len(events) = %d, want 2 (one Scheduled, one Failed)", len(evs))
	}
}

func TestCallTool_CtxCancelAbortsRetry(t *testing.T) {
	ft := &flakyTool{name: "flaky", failN: 100} // always transient
	reg := step.NewRegistry(ft)
	ctx, log := newToolsCtx(t, reg)

	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := step.CallTool(cctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "flaky",
		Idempotent: true, MaxAttempts: 5, Backoff: tinyBackoff,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Tool should have run at least once; far fewer than 5 times since
	// cancellation interrupts a backoff.
	if got := atomic.LoadInt32(&ft.count); got >= 5 {
		t.Fatalf("executions = %d, cancellation did not interrupt retry loop", got)
	}
	// The log must contain at least one Scheduled+Failed pair.
	evs := readAllTools(t, log)
	if len(evs) < 2 || evs[0].Kind != event.KindToolCallScheduled {
		t.Fatalf("events = %+v", evs)
	}
}

func TestCallTool_RetryReplayMatches(t *testing.T) {
	// Record a retry-success run.
	liveTool := &flakyTool{name: "flaky", failN: 2}
	liveReg := step.NewRegistry(liveTool)
	liveLog := eventlog.NewInMemory()
	t.Cleanup(func() { _ = liveLog.Close() })
	liveCtx := step.WithContext(context.Background(), step.MustNewContext(step.Config{
		Log: liveLog, RunID: "rec-retry", Tools: liveReg,
	}))
	if _, err := step.CallTool(liveCtx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "flaky",
		Idempotent: true, MaxAttempts: 3, Backoff: noBackoff,
	}); err != nil {
		t.Fatalf("live CallTool: %v", err)
	}
	liveEvs, err := liveLog.Read(context.Background(), "rec-retry")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// Replay: fresh counter (starts at 0) reproduces the same
	// transient-transient-success pattern.
	replayTool := &flakyTool{name: "flaky", failN: 2}
	replayReg := step.NewRegistry(replayTool)
	replayLog := eventlog.NewInMemory()
	t.Cleanup(func() { _ = replayLog.Close() })
	replayCtxValue := step.MustNewContext(step.Config{
		Log: replayLog, RunID: "replay-retry", Tools: replayReg,
		Mode:     step.ModeReplay,
		Recorded: liveEvs,
	})
	rCtx := step.WithContext(context.Background(), replayCtxValue)
	if _, err := step.CallTool(rCtx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "flaky",
		Idempotent: true, MaxAttempts: 3, Backoff: noBackoff,
	}); err != nil {
		t.Fatalf("replay CallTool: %v", err)
	}
}

func TestCallTools_ParallelRetryOrdering(t *testing.T) {
	// One flaky tool (retries) and one clean echo tool running in
	// parallel. replayCompletionOrder must order by each CallID's
	// FINAL outcome, not first attempt.
	flaky := &flakyTool{name: "flaky", failN: 1}
	reg := step.NewRegistry(flaky, echoTool())
	ctx, log := newToolsCtx(t, reg)

	results, err := step.CallTools(ctx, []step.ToolCall{
		{CallID: "cf", TurnID: "t1", Name: "flaky",
			Idempotent: true, MaxAttempts: 3, Backoff: noBackoff},
		{CallID: "ce", TurnID: "t1", Name: "echo",
			Args: json.RawMessage(`{"msg":"hi"}`)},
	})
	if err != nil {
		t.Fatalf("CallTools: %v", err)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("results[%d].Err = %v", i, r.Err)
		}
	}

	// The flaky tool's first Failed lands early; its final Completed
	// lands later. The echo's Completed lands somewhere between them.
	// We assert only that each CallID has at least one Scheduled and a
	// final Completed event.
	evs := readAllTools(t, log)
	var flakyScheduled, flakyCompleted, echoCompleted int
	for _, ev := range evs {
		switch ev.Kind {
		case event.KindToolCallScheduled:
			s, _ := ev.AsToolCallScheduled()
			if s.CallID == "cf" {
				flakyScheduled++
			}
		case event.KindToolCallCompleted:
			c, _ := ev.AsToolCallCompleted()
			if c.CallID == "cf" {
				flakyCompleted++
			}
			if c.CallID == "ce" {
				echoCompleted++
			}
		}
	}
	if flakyScheduled != 2 {
		t.Fatalf("flaky Scheduled count = %d, want 2", flakyScheduled)
	}
	if flakyCompleted != 1 || echoCompleted != 1 {
		t.Fatalf("Completed: flaky=%d echo=%d, want 1 and 1", flakyCompleted, echoCompleted)
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

func TestCallTool_RejectsTrailingJSON(t *testing.T) {
	reg := step.NewRegistry(echoTool())
	ctx, _ := newToolsCtx(t, reg)

	// `{"msg":"hi"}{"msg":"hi"}` is two concatenated JSON values; the
	// stricter decoder must reject the second.
	args := json.RawMessage(`{"msg":"hi"}{"msg":"x"}`)
	_, err := step.CallTool(ctx, step.ToolCall{
		CallID: "c1", TurnID: "t1", Name: "echo", Args: args,
	})
	if err == nil {
		t.Fatalf("CallTool with trailing JSON accepted; want error")
	}
	if !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("err = %v, want \"trailing data\" diagnostic", err)
	}
}
