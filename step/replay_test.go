package step_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/step"
)

// recordRun runs a fresh live-mode Context through fn and returns the
// resulting event stream — used to seed a replay-mode Context.
func recordRun(t *testing.T, fn func(ctx context.Context)) []event.Event {
	t.Helper()
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	c := step.MustNewContext(step.Config{Log: log, RunID: "rec"})
	fn(step.WithContext(context.Background(), c))
	evs, err := log.Read(context.Background(), "rec")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return evs
}

// replayCtx builds a replay-mode step.Context over evs. ClockFn is
// wired to panic so tests can prove it was never consulted.
func replayCtx(t *testing.T, evs []event.Event) (context.Context, eventlog.EventLog) {
	t.Helper()
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	c := step.MustNewContext(step.Config{
		Log:      log,
		RunID:    "replay",
		Mode:     step.ModeReplay,
		Recorded: evs,
		ClockFn: func() time.Time {
			panic("ClockFn must not be invoked in replay mode")
		},
	})
	return step.WithContext(context.Background(), c), log
}

func TestReplay_NowReturnsRecordedValues(t *testing.T) {
	var t1, t2, t3 time.Time
	evs := recordRun(t, func(ctx context.Context) {
		t1 = step.Now(ctx)
		t2 = step.Now(ctx)
		t3 = step.Now(ctx)
	})
	if len(evs) != 3 {
		t.Fatalf("len(evs) = %d, want 3", len(evs))
	}

	ctx, log := replayCtx(t, evs)
	r1 := step.Now(ctx)
	r2 := step.Now(ctx)
	r3 := step.Now(ctx)

	// time.Unix(0, n) reconstructs from nanos; compare nanos directly
	// since the original may have a non-UTC location attached.
	if r1.UnixNano() != t1.UnixNano() || r2.UnixNano() != t2.UnixNano() || r3.UnixNano() != t3.UnixNano() {
		t.Fatalf("replayed times differ:\n got  %d %d %d\n want %d %d %d",
			r1.UnixNano(), r2.UnixNano(), r3.UnixNano(),
			t1.UnixNano(), t2.UnixNano(), t3.UnixNano())
	}

	// Replay re-emits each recorded SideEffectRecorded into the sink so
	// the re-executed chain byte-matches the original. Verify the
	// kinds and payloads line up with the recording.
	out, _ := log.Read(context.Background(), "replay")
	if len(out) != len(evs) {
		t.Fatalf("replay emitted %d events, want %d", len(out), len(evs))
	}
	for i, ev := range out {
		if ev.Kind != event.KindSideEffectRecorded {
			t.Fatalf("out[%d].Kind = %s, want SideEffectRecorded", i, ev.Kind)
		}
		if string(ev.Payload) != string(evs[i].Payload) {
			t.Fatalf("out[%d].Payload mismatch", i)
		}
	}
}

func TestReplay_RandomReturnsRecordedValue(t *testing.T) {
	var v uint64
	evs := recordRun(t, func(ctx context.Context) {
		v = step.Random(ctx)
	})

	ctx, _ := replayCtx(t, evs)
	if got := step.Random(ctx); got != v {
		t.Fatalf("replay Random = %d, want %d", got, v)
	}
}

func TestReplay_SideEffectReturnsRecordedValue(t *testing.T) {
	type payload struct {
		Who string
		N   int
	}
	evs := recordRun(t, func(ctx context.Context) {
		_, err := step.SideEffect(ctx, "whoami", func() (payload, error) {
			return payload{Who: "alice", N: 42}, nil
		})
		if err != nil {
			t.Fatalf("SideEffect: %v", err)
		}
	})

	ctx, _ := replayCtx(t, evs)
	calls := 0
	got, err := step.SideEffect(ctx, "whoami", func() (payload, error) {
		calls++
		return payload{}, nil
	})
	if err != nil {
		t.Fatalf("SideEffect: %v", err)
	}
	if calls != 0 {
		t.Fatalf("fn invoked in replay mode")
	}
	if got.Who != "alice" || got.N != 42 {
		t.Fatalf("got = %+v, want {alice 42}", got)
	}
}

func TestReplay_MismatchPanicsWithReplayMismatch(t *testing.T) {
	// Record one Random; replay as Now — expect a panic wrapping
	// ErrReplayMismatch.
	evs := recordRun(t, func(ctx context.Context) {
		_ = step.Random(ctx)
	})
	ctx, _ := replayCtx(t, evs)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, step.ErrReplayMismatch.Error()) {
			t.Fatalf("panic msg = %q, want substring %q", msg, step.ErrReplayMismatch.Error())
		}
	}()
	_ = step.Now(ctx)
}

func TestReplay_ExhaustedStreamPanics(t *testing.T) {
	evs := recordRun(t, func(ctx context.Context) {
		_ = step.Now(ctx)
	})
	ctx, _ := replayCtx(t, evs)

	_ = step.Now(ctx) // consumes the one recorded event

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on exhausted stream")
		}
		if !strings.Contains(fmt.Sprint(r), "replay stream exhausted") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	_ = step.Now(ctx)
}

// TestReplay_NdetHelperAdvancesChain is the regression test for the
// bug where step.Now (and siblings) didn't advance nextSeq in replay,
// causing the next real emit to see a stale SideEffectRecorded slot.
// A recorded [SideEffectRecorded, RunCompleted] replay must reach the
// RunCompleted emit without an ErrReplayMismatch.
func TestReplay_NdetHelperAdvancesChain(t *testing.T) {
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	rec := step.MustNewContext(step.Config{Log: log, RunID: "rec"})
	recCtx := step.WithContext(context.Background(), rec)

	_ = step.Now(recCtx)
	if err := step.Emit(recCtx, rec, event.KindRunCompleted, event.RunCompleted{FinalText: "ok"}); err != nil {
		t.Fatalf("Emit RunCompleted: %v", err)
	}
	evs, err := log.Read(context.Background(), "rec")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(evs) != 2 || evs[0].Kind != event.KindSideEffectRecorded || evs[1].Kind != event.KindRunCompleted {
		t.Fatalf("unexpected recording: %+v", evs)
	}

	sink := eventlog.NewInMemory()
	t.Cleanup(func() { _ = sink.Close() })
	rp := step.MustNewContext(step.Config{
		Log:      sink,
		RunID:    "replay",
		Mode:     step.ModeReplay,
		Recorded: evs,
		ClockFn:  func() time.Time { panic("clock used in replay") },
	})
	rpCtx := step.WithContext(context.Background(), rp)

	_ = step.Now(rpCtx)
	if err := step.Emit(rpCtx, rp, event.KindRunCompleted, event.RunCompleted{FinalText: "ok"}); err != nil {
		t.Fatalf("replay Emit RunCompleted: %v (pre-fix: ErrReplayMismatch because Now did not advance nextSeq)", err)
	}

	out, _ := sink.Read(context.Background(), "replay")
	if len(out) != len(evs) {
		t.Fatalf("replay sink got %d events, want %d", len(out), len(evs))
	}
	for i, ev := range out {
		if ev.Kind != evs[i].Kind || string(ev.Payload) != string(evs[i].Payload) {
			t.Fatalf("out[%d] diverges from recording", i)
		}
	}
}

func TestReplay_ErrReplayMismatchIsExported(t *testing.T) {
	// Sanity: the error value is reachable and an actual error.
	if step.ErrReplayMismatch == nil {
		t.Fatal("ErrReplayMismatch is nil")
	}
	if !errors.Is(step.ErrReplayMismatch, step.ErrReplayMismatch) {
		t.Fatal("errors.Is self-check failed")
	}
}

func TestNewContext_ModeReplayWithoutRecordedPanics(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = step.MustNewContext(step.Config{
		Log:   log,
		RunID: "x",
		Mode:  step.ModeReplay,
	})
}

func TestLive_ClockFnOverride(t *testing.T) {
	// ClockFn in live mode replaces time.Now. Useful for testing
	// downstream consumers that key on timestamps.
	fixed := time.Unix(0, 1_700_000_000_000_000_000)
	log := eventlog.NewInMemory()
	defer log.Close()
	c := step.MustNewContext(step.Config{
		Log:     log,
		RunID:   "fc",
		ClockFn: func() time.Time { return fixed },
	})
	ctx := step.WithContext(context.Background(), c)

	got := step.Now(ctx)
	if got.UnixNano() != fixed.UnixNano() {
		t.Fatalf("got = %v, want %v", got, fixed)
	}
}
