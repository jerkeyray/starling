// Stream — channel-based live run API.
//
// Stream starts a new agent run and yields one StepEvent per emitted
// event on the returned channel. Terminal events (RunCompleted,
// RunFailed, RunCancelled) are always delivered as the final item
// before the channel closes. A consumer wanting per-event reactions
// (dashboards, real-time supervisors, SSE fan-out inside a larger
// server) can wire it up in one call rather than subscribing to
// eventlog.Stream and re-implementing projection.
//
// The shape mirrors replay.Stream: subscribe to the log BEFORE kicking
// off the run so no event can land ahead of the reader; forward events
// through a small-buffered channel; close on terminal, ctx cancel, or
// log close.

package starling

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerkeyray/starling/event"
)

// streamBufferSize is the capacity of the StepEvent channel returned
// by Stream. Slow consumers risk being dropped by the underlying
// eventlog subscriber (in-memory drops on overflow; the 256-slot log
// buffer backs this 64-slot projector, which is generous for any
// hand-written tool set). Consumers should drain promptly.
const streamBufferSize = 64

// Stream starts a new agent run and returns a channel delivering one
// StepEvent per emitted event. Terminal events are always the last
// item on the channel before it closes.
//
// The channel has a small buffer; slow consumers risk losing events
// if the eventlog's subscriber buffer overflows (see
// eventlog.EventLog.Stream documentation). The channel still closes
// cleanly in that case, so the contract "channel always closes" holds
// regardless of consumer speed.
//
// On ctx cancel the agent run is cancelled (standard ctx plumbing)
// and the channel closes after draining any in-flight events.
//
// Setup errors (validate failure, subscribe failure) are returned
// synchronously and the returned channel is nil. Run-time errors
// surface as a terminal StepEvent (Kind=RunFailed / RunCancelled,
// Err populated) and then the channel closes — Stream itself does
// not return a second error.
func (a *Agent) Stream(ctx context.Context, goal string) (string, <-chan StepEvent, error) {
	if err := a.validate(); err != nil {
		return "", nil, err
	}
	runID := a.mintRunID()

	// Subscribe BEFORE starting the run so no event can land before
	// our reader is up. Same pattern as replay/stream.go.
	raw, err := a.Log.Stream(ctx, runID)
	if err != nil {
		return "", nil, fmt.Errorf("starling: Stream subscribe: %w", err)
	}

	out := make(chan StepEvent, streamBufferSize)
	runErrCh := make(chan error, 1)

	go func() {
		_, rerr := a.runWithID(ctx, runID, goal)
		runErrCh <- rerr
	}()

	go func() {
		defer close(out)
		for {
			select {
			case ev, ok := <-raw:
				if !ok {
					// log.Stream closed (log closed / ctx cancel / slow
					// subscriber drop). Drain the run error slot and
					// exit.
					select {
					case <-runErrCh:
					default:
					}
					return
				}
				se := toStepEvent(ev)
				select {
				case out <- se:
				case <-ctx.Done():
					return
				}
				// Terminal events are always the last event a run
				// emits (agent.Run contract). Close the stream as
				// soon as we forward one — log.Stream itself does
				// not close the subscriber channel at end-of-run, so
				// without this check the channel would hang until
				// ctx cancel.
				if ev.Kind.IsTerminal() {
					select {
					case <-runErrCh:
					case <-ctx.Done():
					}
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return runID, out, nil
}

// toStepEvent projects a raw event.Event into the narrower StepEvent
// shape exposed by Stream. Pure function; payload decode errors
// degrade gracefully (projected fields zero, Raw still populated) so
// a schema surprise never panics the streaming goroutine.
func toStepEvent(ev event.Event) StepEvent {
	se := StepEvent{Kind: ev.Kind, Raw: ev}
	switch ev.Kind {
	case event.KindRunStarted:
		if p, err := ev.AsRunStarted(); err == nil {
			se.Text = p.Goal
		}
	case event.KindTurnStarted:
		if p, err := ev.AsTurnStarted(); err == nil {
			se.TurnID = p.TurnID
		}
	case event.KindReasoningEmitted:
		if p, err := ev.AsReasoningEmitted(); err == nil {
			se.TurnID = p.TurnID
			se.Text = p.Content
		}
	case event.KindAssistantMessageCompleted:
		if p, err := ev.AsAssistantMessageCompleted(); err == nil {
			se.TurnID = p.TurnID
			se.Text = p.Text
		}
	case event.KindToolCallScheduled:
		if p, err := ev.AsToolCallScheduled(); err == nil {
			se.TurnID = p.TurnID
			se.CallID = p.CallID
			se.Tool = p.ToolName
		}
	case event.KindToolCallCompleted:
		if p, err := ev.AsToolCallCompleted(); err == nil {
			se.CallID = p.CallID
			se.Text = string(p.Result)
		}
	case event.KindToolCallFailed:
		if p, err := ev.AsToolCallFailed(); err == nil {
			se.CallID = p.CallID
			if p.Error != "" {
				se.Err = errors.New(p.Error)
			}
		}
	case event.KindBudgetExceeded:
		if p, err := ev.AsBudgetExceeded(); err == nil {
			se.TurnID = p.TurnID
			se.CallID = p.CallID
		}
		se.Err = ErrBudgetExceeded
	case event.KindContextTruncated:
		// No TurnID on the payload today; leave blank.
	case event.KindRunCompleted:
		if p, err := ev.AsRunCompleted(); err == nil {
			se.Text = p.FinalText
		}
	case event.KindRunFailed:
		if p, err := ev.AsRunFailed(); err == nil && p.Error != "" {
			se.Err = errors.New(p.Error)
		}
	case event.KindRunCancelled:
		se.Err = context.Canceled
	}
	return se
}
