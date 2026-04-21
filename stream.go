// Channel-based live run API. Mirrors replay.Stream: subscribe to
// the log before starting the run so nothing lands ahead of the
// reader; close on terminal, ctx cancel, or log close.

package starling

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerkeyray/starling/event"
)

// Capacity of the StepEvent channel. Slow consumers risk being
// dropped by the underlying eventlog subscriber.
const streamBufferSize = 64

// Stream starts a new run and returns a channel of StepEvents.
// Terminal events are always last. Setup errors are returned
// synchronously; run-time errors surface as a terminal StepEvent
// (Err populated). On ctx cancel the run is cancelled and the
// channel closes after draining.
func (a *Agent) Stream(ctx context.Context, goal string) (string, <-chan StepEvent, error) {
	if err := a.validate(); err != nil {
		return "", nil, err
	}
	runID := a.mintRunID()

	// Subscribe before starting the run.
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
					// log.Stream closed. Drain the run error slot.
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
				// Close on terminal; log.Stream won't close the
				// subscriber at end-of-run.
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
