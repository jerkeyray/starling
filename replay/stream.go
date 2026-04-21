// Channel-based replay execution. On divergence, emits a final
// ReplayStep with Diverged=true and closes the channel
// (first-divergence-wins).

package replay

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/step"
)

// Factory builds a fresh Agent for a replay session. The inspector
// invokes this once per session so the user controls how their
// provider config / tool registry / namespace are wired. Returning an
// error aborts the session before any events are streamed.
type Factory func(ctx context.Context) (Agent, error)

// StreamingAgent is the Agent contract Stream needs: the existing
// RunReplay shape plus a sink-accepting variant so Stream can
// observe the in-process replay log without owning its lifecycle.
//
// *starling.Agent satisfies StreamingAgent.
type StreamingAgent interface {
	Agent
	RunReplayInto(ctx context.Context, recorded []event.Event, sink eventlog.EventLog) error
}

// ReplayStep is one item in the streamed timeline. For matching
// events Recorded and Produced carry the same logical event (same
// Kind, same Payload bytes — Produced is the freshly re-emitted copy,
// Recorded is from the source log). For the divergence step Produced
// is the zero value and DivergenceReason carries the explanation.
type ReplayStep struct {
	// Index is the zero-based position in the recorded event slice.
	Index uint64
	// Recorded is the event at recorded[Index]. Always populated.
	Recorded event.Event
	// Produced is the event the re-executed agent emitted at this
	// position. For matching steps it equals Recorded byte-for-byte
	// (up to the live Timestamp, which replay reuses from Recorded so
	// the chain hash sequence is identical). For the divergence step
	// it is the zero value.
	Produced event.Event
	// Diverged is true on the final step when the re-executed run
	// failed to reproduce the recording. Set on at most one step.
	Diverged bool
	// DivergenceReason is a one-line human-readable explanation of
	// what went wrong, suitable for display in the inspector. Empty
	// unless Diverged is true.
	DivergenceReason string
}

// Stream re-executes runID against factory's agent and yields a
// ReplayStep per emitted event on the returned channel. The channel
// closes when the run completes (clean or divergent) or when ctx is
// cancelled.
//
// The caller must drain the channel; Stream uses a small buffer
// (size 16) and a slow consumer will backpressure the replay run.
//
// log is the source of truth for the recording. factory builds the
// agent that re-executes; it must be configured equivalently to the
// agent that originally produced the run (same provider ID, same
// tool set, same namespace). A first-event divergence almost always
// indicates a factory mismatch — the DivergenceReason is intended to
// surface enough context that the user can spot the gap.
func Stream(ctx context.Context, factory Factory, log eventlog.EventLog, runID string) (<-chan ReplayStep, error) {
	if factory == nil {
		return nil, errors.New("replay: Stream factory is nil")
	}
	if log == nil {
		return nil, errors.New("replay: Stream log is nil")
	}
	recorded, err := log.Read(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("replay: read log: %w", err)
	}
	if len(recorded) == 0 {
		return nil, fmt.Errorf("replay: run %q not found", runID)
	}

	a, err := factory(ctx)
	if err != nil {
		return nil, fmt.Errorf("replay: build agent: %w", err)
	}
	sa, ok := a.(StreamingAgent)
	if !ok {
		return nil, errors.New("replay: agent does not implement RunReplayInto (need *starling.Agent or compatible)")
	}

	// Sink captures every byte-matching event the re-executed agent
	// appends. We subscribe BEFORE kicking off the run so no event
	// can land before our reader is up.
	sink := eventlog.NewInMemory()
	produced, err := sink.Stream(ctx, recorded[0].RunID)
	if err != nil {
		sink.Close()
		return nil, fmt.Errorf("replay: subscribe to sink: %w", err)
	}

	out := make(chan ReplayStep, 16)
	runErrCh := make(chan error, 1)

	// Run goroutine. RunReplayInto returns when the agent loop exits
	// (terminal event or first divergence error). Closing the sink
	// here is what causes `produced` to close in the streaming
	// goroutine — without it the stream consumer would block forever
	// after the last event.
	go func() {
		err := sa.RunReplayInto(ctx, recorded, sink)
		runErrCh <- err
		// sink.Close drains the in-flight events to subscribers and
		// then closes the produced channel. Safe to call before the
		// streaming goroutine has finished consuming — Stream guarantees
		// historical events are delivered before the close.
		_ = sink.Close()
	}()

	// Streaming goroutine. Pairs each produced event with the same
	// index of recorded; once produced closes, synthesises any final
	// divergence step from runErrCh.
	go func() {
		defer close(out)

		var idx uint64
		for ev := range produced {
			step := ReplayStep{
				Index:    idx,
				Recorded: pickRecorded(recorded, idx),
				Produced: ev,
			}
			select {
			case out <- step:
			case <-ctx.Done():
				return
			}
			idx++
		}
		emitFinalDivergence(ctx, out, runErrCh, recorded, idx)
	}()

	return out, nil
}

// pickRecorded returns recorded[idx] or the zero value when idx is
// past the end of the slice (the produced run emitted more events
// than the recording — a divergence in itself, surfaced by
// emitFinalDivergence).
func pickRecorded(recorded []event.Event, idx uint64) event.Event {
	if idx >= uint64(len(recorded)) {
		return event.Event{}
	}
	return recorded[idx]
}

// emitFinalDivergence is called once the produced channel closes. It
// inspects runErrCh and synthesizes the divergence ReplayStep, if
// any. Three terminal cases:
//
//  1. Clean replay: runErr is nil and idx == len(recorded). No final
//     step needed.
//  2. ErrReplayMismatch: the agent loop bailed mid-run; emit a
//     divergence step at idx with the error message as the reason.
//  3. Other run error (tool failure, ctx cancel, …): emit a
//     divergence step with the wrapped error as the reason. The UI
//     treats it the same as a mismatch: red row, click to expand.
func emitFinalDivergence(
	ctx context.Context,
	out chan<- ReplayStep,
	runErrCh <-chan error,
	recorded []event.Event,
	idx uint64,
) {
	var runErr error
	select {
	case runErr = <-runErrCh:
	case <-ctx.Done():
		return
	}
	if runErr == nil && idx == uint64(len(recorded)) {
		return // clean
	}
	reason := "produced fewer events than recorded"
	if runErr != nil {
		switch {
		case errors.Is(runErr, step.ErrReplayMismatch):
			reason = runErr.Error()
		default:
			reason = "run error: " + runErr.Error()
		}
	}
	final := ReplayStep{
		Index:            idx,
		Recorded:         pickRecorded(recorded, idx),
		Diverged:         true,
		DivergenceReason: reason,
	}
	select {
	case out <- final:
	case <-ctx.Done():
	}
}
