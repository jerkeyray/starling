package step_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/step"
)

// fixture wires an in-memory log + fresh Context + ctx carrying it.
// Returns everything the test needs; callers close the log via t.Cleanup.
func fixture(t *testing.T) (context.Context, *step.Context, eventlog.EventLog) {
	t.Helper()
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	c := step.MustNewContext(step.Config{Log: log, RunID: "run-test-1"})
	ctx := step.WithContext(context.Background(), c)
	return ctx, c, log
}

func TestFrom_MissingReturnsFalse(t *testing.T) {
	if c, ok := step.From(context.Background()); ok || c != nil {
		t.Fatalf("From(bg) = (%v, %v), want (nil, false)", c, ok)
	}
}

func TestWithContext_RoundTrip(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	want := step.MustNewContext(step.Config{Log: log, RunID: "run-1"})
	ctx := step.WithContext(context.Background(), want)
	got, ok := step.From(ctx)
	if !ok || got != want {
		t.Fatalf("From(with(ctx,want)) = (%p, %v), want (%p, true)", got, ok, want)
	}
}

func TestContext_RunID(t *testing.T) {
	_, c, _ := fixture(t)
	if got := c.RunID(); got != "run-test-1" {
		t.Fatalf("RunID = %q, want run-test-1", got)
	}
}

func TestNow_EmitsMonotonicAndChains(t *testing.T) {
	ctx, _, log := fixture(t)

	t1 := step.Now(ctx)
	t2 := step.Now(ctx)
	if t2.Before(t1) {
		t.Fatalf("wall clock went backwards: %v then %v", t1, t2)
	}

	evs, err := log.Read(context.Background(), "run-test-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(evs))
	}

	// Shape assertions: seq chain, kind, name, and monotonic payload values.
	if evs[0].Seq != 1 || evs[1].Seq != 2 {
		t.Fatalf("seq chain = [%d, %d], want [1, 2]", evs[0].Seq, evs[1].Seq)
	}
	if len(evs[0].PrevHash) != 0 {
		t.Fatalf("first PrevHash not empty: %x", evs[0].PrevHash)
	}
	wantPrev := event.Hash(mustMarshal(t, evs[0]))
	if string(evs[1].PrevHash) != string(wantPrev) {
		t.Fatalf("PrevHash mismatch:\n got  %x\n want %x", evs[1].PrevHash, wantPrev)
	}

	var v1, v2 int64
	for i, ev := range evs {
		se, err := ev.AsSideEffectRecorded()
		if err != nil {
			t.Fatalf("event[%d] AsSideEffectRecorded: %v", i, err)
		}
		if se.Name != "now" {
			t.Fatalf("event[%d].Name = %q, want now", i, se.Name)
		}
		var nanos int64
		if err := cborenc.Unmarshal(se.Value, &nanos); err != nil {
			t.Fatalf("event[%d] decode value: %v", i, err)
		}
		if i == 0 {
			v1 = nanos
		} else {
			v2 = nanos
		}
	}
	if v2 < v1 {
		t.Fatalf("payload timestamps not monotonic: %d then %d", v1, v2)
	}
}

func TestRandom_EmitsEvent(t *testing.T) {
	ctx, _, log := fixture(t)

	v := step.Random(ctx)

	evs, _ := log.Read(context.Background(), "run-test-1")
	if len(evs) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(evs))
	}
	se, err := evs[0].AsSideEffectRecorded()
	if err != nil {
		t.Fatalf("AsSideEffectRecorded: %v", err)
	}
	if se.Name != "rand" {
		t.Fatalf("Name = %q, want rand", se.Name)
	}
	var decoded uint64
	if err := cborenc.Unmarshal(se.Value, &decoded); err != nil {
		t.Fatalf("decode value: %v", err)
	}
	if decoded != v {
		t.Fatalf("recorded %d, returned %d", decoded, v)
	}
}

func TestSideEffect_HappyPath(t *testing.T) {
	ctx, _, log := fixture(t)

	out, err := step.SideEffect(ctx, "whoami", func() (string, error) {
		return "alice", nil
	})
	if err != nil {
		t.Fatalf("SideEffect: %v", err)
	}
	if out != "alice" {
		t.Fatalf("out = %q", out)
	}

	evs, _ := log.Read(context.Background(), "run-test-1")
	if len(evs) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(evs))
	}
	se, _ := evs[0].AsSideEffectRecorded()
	if se.Name != "whoami" {
		t.Fatalf("Name = %q", se.Name)
	}
	var decoded string
	if err := cborenc.Unmarshal(se.Value, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded != "alice" {
		t.Fatalf("decoded = %q", decoded)
	}
}

func TestSideEffect_ErrorNoEvent(t *testing.T) {
	ctx, _, log := fixture(t)

	sentinel := errors.New("boom")
	_, err := step.SideEffect(ctx, "doomed", func() (int, error) {
		return 0, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	evs, _ := log.Read(context.Background(), "run-test-1")
	if len(evs) != 0 {
		t.Fatalf("len(events) = %d, want 0 (no event on fn error)", len(evs))
	}
}

func TestNow_PanicsWithoutContext(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic")
		}
	}()
	_ = step.Now(context.Background())
}

func TestRandom_PanicsWithoutContext(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic")
		}
	}()
	_ = step.Random(context.Background())
}

func TestSideEffect_PanicsWithoutContext(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic")
		}
	}()
	_, _ = step.SideEffect(context.Background(), "x", func() (int, error) { return 0, nil })
}

// mustMarshal wraps event.Marshal for test assertions.
func mustMarshal(t *testing.T, ev event.Event) []byte {
	t.Helper()
	b, err := event.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

