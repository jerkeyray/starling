package replay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/step"
)

// fakeAgent is a StreamingAgent that re-appends a configurable
// prefix of the recorded events to the sink, then optionally returns
// a synthetic divergence error. It's the smallest harness that lets
// us pin Stream's channel + final-step semantics without spinning up
// a real *starling.Agent (which would import-cycle this test file).
type fakeAgent struct {
	// emitN is the number of recorded events to append to the sink
	// before stopping. -1 means all of them (clean replay).
	emitN int
	// finalErr is what RunReplayInto returns. nil for clean.
	finalErr error
}

func (f *fakeAgent) RunReplay(ctx context.Context, recorded []event.Event) error {
	return errors.New("not used in stream tests")
}

func (f *fakeAgent) RunReplayInto(ctx context.Context, recorded []event.Event, sink eventlog.EventLog) error {
	n := f.emitN
	if n < 0 || n > len(recorded) {
		n = len(recorded)
	}
	for i := 0; i < n; i++ {
		if err := sink.Append(ctx, recorded[i].RunID, recorded[i]); err != nil {
			return err
		}
	}
	return f.finalErr
}

// seedRun appends a minimal three-event run (RunStarted, TurnStarted,
// RunCompleted) to log and returns the recorded slice. The events
// only need to round-trip through the log so Stream has something to
// compare against; we don't run the agent loop here.
func seedRun(t *testing.T, log eventlog.EventLog, runID string) []event.Event {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixNano()

	type kp struct {
		kind    event.Kind
		payload any
	}
	steps := []kp{
		{event.KindRunStarted, event.RunStarted{
			SchemaVersion: event.SchemaVersion,
			Goal:          "test",
			ProviderID:    "fake",
			APIVersion:    "v1",
			ModelID:       "m",
		}},
		{event.KindTurnStarted, event.TurnStarted{TurnID: "t1"}},
		{event.KindRunCompleted, event.RunCompleted{FinalText: "ok", TurnCount: 1}},
	}

	var prev []byte
	for i, s := range steps {
		encoded, err := encodeAny(s.payload)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		ev := event.Event{
			RunID:     runID,
			Seq:       uint64(i + 1),
			PrevHash:  prev,
			Timestamp: now + int64(i),
			Kind:      s.kind,
			Payload:   encoded,
		}
		if err := log.Append(ctx, runID, ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		marshaled, err := event.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		prev = event.Hash(marshaled)
	}
	got, err := log.Read(ctx, runID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

// encodeAny is a tiny shim around the generic event.EncodePayload so
// the heterogeneous step table in seedRun stays readable.
func encodeAny(p any) ([]byte, error) {
	switch v := p.(type) {
	case event.RunStarted:
		b, err := event.EncodePayload(v)
		return []byte(b), err
	case event.TurnStarted:
		b, err := event.EncodePayload(v)
		return []byte(b), err
	case event.RunCompleted:
		b, err := event.EncodePayload(v)
		return []byte(b), err
	}
	return nil, fmt.Errorf("encodeAny: unsupported %T", p)
}

func factoryReturning(a StreamingAgent) Factory {
	return func(_ context.Context) (Agent, error) { return a, nil }
}

// drainSteps reads from ch until it closes (or the test times out)
// and returns every step. Bounded so a buggy Stream that never closes
// fails the test instead of hanging it.
func drainSteps(t *testing.T, ch <-chan ReplayStep) []ReplayStep {
	t.Helper()
	var got []ReplayStep
	timeout := time.After(2 * time.Second)
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, s)
		case <-timeout:
			t.Fatalf("Stream did not close after 2s; got %d steps so far", len(got))
		}
	}
}

func TestStream_CleanReplay_NoDivergence(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	recorded := seedRun(t, log, "r-clean")

	ch, err := Stream(context.Background(), factoryReturning(&fakeAgent{emitN: -1}), log, "r-clean")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := drainSteps(t, ch)

	if len(got) != len(recorded) {
		t.Fatalf("got %d steps, want %d", len(got), len(recorded))
	}
	for i, s := range got {
		if s.Diverged {
			t.Errorf("step %d unexpectedly diverged: %s", i, s.DivergenceReason)
		}
		if s.Index != uint64(i) {
			t.Errorf("step %d: Index=%d, want %d", i, s.Index, i)
		}
		if s.Recorded.Seq != recorded[i].Seq {
			t.Errorf("step %d: Recorded.Seq=%d, want %d", i, s.Recorded.Seq, recorded[i].Seq)
		}
		if s.Produced.Seq != recorded[i].Seq {
			t.Errorf("step %d: Produced.Seq=%d, want %d", i, s.Produced.Seq, recorded[i].Seq)
		}
	}
}

func TestStream_MidRunDivergence_FinalStepFlagged(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	recorded := seedRun(t, log, "r-mid")

	// Agent emits the first event then bails with a mismatch error
	// at index 1 — same shape as the real emit() bailing in replay
	// mode when payload bytes diverge.
	mismatchErr := fmt.Errorf("%w: seq=2 payload mismatch (kind=TurnStarted)", step.ErrReplayMismatch)
	ch, err := Stream(context.Background(), factoryReturning(&fakeAgent{emitN: 1, finalErr: mismatchErr}), log, "r-mid")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := drainSteps(t, ch)

	if len(got) != 2 {
		t.Fatalf("got %d steps, want 2 (1 matching + 1 divergence)", len(got))
	}
	if got[0].Diverged {
		t.Errorf("step 0 should not have diverged")
	}
	final := got[1]
	if !final.Diverged {
		t.Fatalf("final step Diverged=false; want true (reason=%q)", final.DivergenceReason)
	}
	if final.Index != 1 {
		t.Errorf("final step Index=%d, want 1", final.Index)
	}
	if final.Recorded.Seq != recorded[1].Seq {
		t.Errorf("final step Recorded.Seq=%d, want %d", final.Recorded.Seq, recorded[1].Seq)
	}
	if final.DivergenceReason == "" {
		t.Errorf("final step DivergenceReason is empty")
	}
}

func TestStream_FirstEventDivergence(t *testing.T) {
	// Most common factory-mismatch case: provider ID different →
	// emit() rejects RunStarted at seq=1. Stream should emit one
	// final divergence step at index 0 with Recorded populated.
	log := eventlog.NewInMemory()
	defer log.Close()
	seedRun(t, log, "r-first")

	mismatchErr := fmt.Errorf("%w: seq=1 payload mismatch (kind=RunStarted)", step.ErrReplayMismatch)
	ch, err := Stream(context.Background(), factoryReturning(&fakeAgent{emitN: 0, finalErr: mismatchErr}), log, "r-first")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := drainSteps(t, ch)

	if len(got) != 1 {
		t.Fatalf("got %d steps, want 1", len(got))
	}
	if !got[0].Diverged {
		t.Fatalf("step 0 should have diverged")
	}
	if got[0].Index != 0 {
		t.Errorf("step 0 Index=%d, want 0", got[0].Index)
	}
}

func TestStream_PartialReplay_NoError_FlagsShortRun(t *testing.T) {
	// Agent silently exits before reproducing every event (e.g. a
	// MaxTurns drop). No error returned, but the recording isn't
	// fully reproduced — Stream surfaces this as a divergence.
	log := eventlog.NewInMemory()
	defer log.Close()
	seedRun(t, log, "r-short")

	ch, err := Stream(context.Background(), factoryReturning(&fakeAgent{emitN: 1, finalErr: nil}), log, "r-short")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := drainSteps(t, ch)

	if len(got) != 2 {
		t.Fatalf("got %d steps, want 2", len(got))
	}
	final := got[1]
	if !final.Diverged {
		t.Fatalf("expected final divergence, got %+v", final)
	}
	if final.DivergenceReason == "" {
		t.Errorf("DivergenceReason empty")
	}
}

func TestStream_UnknownRun_ReturnsError(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	_, err := Stream(context.Background(), factoryReturning(&fakeAgent{}), log, "nope")
	if err == nil {
		t.Fatalf("Stream: want error for unknown run")
	}
}

func TestStream_NilFactory_ReturnsError(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	_, err := Stream(context.Background(), nil, log, "anything")
	if err == nil {
		t.Fatalf("Stream: want error for nil factory")
	}
}

func TestStream_FactoryError_Propagates(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	seedRun(t, log, "r-fac")

	want := errors.New("factory boom")
	bad := func(_ context.Context) (Agent, error) { return nil, want }
	_, err := Stream(context.Background(), bad, log, "r-fac")
	if !errors.Is(err, want) {
		t.Fatalf("Stream err = %v, want wraps %v", err, want)
	}
}

func TestStream_FactoryReturnsNonStreamingAgent_ReturnsError(t *testing.T) {
	// An Agent that satisfies the base Agent interface but not
	// StreamingAgent must be rejected up front, not after the run
	// starts.
	log := eventlog.NewInMemory()
	defer log.Close()
	seedRun(t, log, "r-bare")

	bare := func(_ context.Context) (Agent, error) {
		return bareAgent{}, nil
	}
	_, err := Stream(context.Background(), bare, log, "r-bare")
	if err == nil {
		t.Fatalf("Stream: want error for non-streaming agent")
	}
}

type bareAgent struct{}

func (bareAgent) RunReplay(_ context.Context, _ []event.Event) error { return nil }

func TestStream_DivergenceCarriesStructuredFields(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	seedRun(t, log, "r-struct")

	mismatchErr := &step.MismatchError{
		Seq:          2,
		Kind:         event.KindTurnStarted,
		ExpectedKind: event.KindRunStarted,
		Class:        step.MismatchKind,
		Reason:       "seq=2 expected kind RunStarted, got TurnStarted",
	}
	ch, err := Stream(context.Background(), factoryReturning(&fakeAgent{emitN: 1, finalErr: mismatchErr}), log, "r-struct")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := drainSteps(t, ch)
	final := got[len(got)-1]
	if final.Divergence == nil {
		t.Fatalf("final.Divergence is nil; want populated structured form")
	}
	if final.Divergence.Class != step.MismatchKind {
		t.Errorf("Divergence.Class = %s, want %s", final.Divergence.Class, step.MismatchKind)
	}
	if final.Divergence.Seq != 2 {
		t.Errorf("Divergence.Seq = %d, want 2", final.Divergence.Seq)
	}
	if final.Divergence.RunID != "r-struct" {
		t.Errorf("Divergence.RunID = %q, want r-struct", final.Divergence.RunID)
	}
}
