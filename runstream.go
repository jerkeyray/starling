package starling

import (
	"context"
	"errors"

	"github.com/jerkeyray/starling/event"
)

// AgentEvent is the typed event surface for RunStream. Values are one
// of TextDelta, ToolCallStarted, ToolCallEnded, or Done. Unknown
// concrete types must be tolerated by callers using a type switch
// with a default branch — additions are not breaking changes within a
// beta cycle.
//
// AgentEvent is layered on top of the lower-level StepEvent stream
// returned by Stream. RunStream is the user-friendly path; Stream is
// the escape hatch for callers who want every event with the full
// envelope.
type AgentEvent interface {
	isAgentEvent()
}

// TextDelta carries an assistant turn's accumulated text. Emitted
// once per AssistantMessageCompleted, not per intra-turn chunk —
// chunk-level streaming is a future addition behind the same
// AgentEvent surface.
type TextDelta struct {
	TurnID string
	Text   string
}

// ToolCallStarted reports that the runtime has scheduled a tool
// invocation. Emitted on KindToolCallScheduled.
type ToolCallStarted struct {
	TurnID string
	CallID string
	Tool   string
}

// ToolCallEnded reports the tool's outcome. Result is the raw JSON
// the tool returned (empty on failure). Err is non-nil on a failed
// call (KindToolCallFailed) and nil on success
// (KindToolCallCompleted).
type ToolCallEnded struct {
	CallID string
	Tool   string
	Result []byte
	Err    error
}

// Done is always the last AgentEvent on the channel. TerminalKind is
// the run's terminal event kind; Err is set on RunFailed and on
// RunCancelled (with context.Canceled).
type Done struct {
	TerminalKind event.Kind
	FinalText    string
	Err          error
}

func (TextDelta) isAgentEvent()       {}
func (ToolCallStarted) isAgentEvent() {}
func (ToolCallEnded) isAgentEvent()   {}
func (Done) isAgentEvent()            {}

// RunStream starts a new run and returns a channel of typed
// AgentEvents. The channel always closes after a single Done; setup
// errors surface synchronously.
//
// RunStream is a thin projection of Stream — every emitted AgentEvent
// is derived from the same underlying StepEvent that Stream would
// have produced, filtered to the typed surface. Use Stream when you
// need the full envelope (raw event, sequence numbers, every Kind);
// use RunStream when you want a stable, narrow API for chat-style
// frontends.
func (a *Agent) RunStream(ctx context.Context, goal string) (string, <-chan AgentEvent, error) {
	runID, raw, err := a.Stream(ctx, goal)
	if err != nil {
		return "", nil, err
	}

	out := make(chan AgentEvent, streamBufferSize)
	go func() {
		defer close(out)
		// Track scheduled tool names by CallID so ToolCallEnded can
		// echo the Tool name (KindToolCallCompleted/Failed payloads
		// don't repeat it).
		toolByCall := map[string]string{}

		for se := range raw {
			ae, terminal := projectAgentEvent(se, toolByCall)
			if ae != nil {
				select {
				case out <- ae:
				case <-ctx.Done():
					return
				}
			}
			if terminal {
				return
			}
		}
	}()

	return runID, out, nil
}

// projectAgentEvent turns a StepEvent into an optional AgentEvent. The
// second return is true when the StepEvent is terminal (a Done event
// has just been emitted, or will be by the caller); the projection
// emits Done itself so callers don't need to remember to.
func projectAgentEvent(se StepEvent, toolByCall map[string]string) (AgentEvent, bool) {
	switch se.Kind {
	case event.KindAssistantMessageCompleted:
		if se.Text == "" {
			return nil, false
		}
		return TextDelta{TurnID: se.TurnID, Text: se.Text}, false
	case event.KindToolCallScheduled:
		toolByCall[se.CallID] = se.Tool
		return ToolCallStarted{TurnID: se.TurnID, CallID: se.CallID, Tool: se.Tool}, false
	case event.KindToolCallCompleted:
		return ToolCallEnded{
			CallID: se.CallID,
			Tool:   toolByCall[se.CallID],
			Result: []byte(se.Text),
		}, false
	case event.KindToolCallFailed:
		err := se.Err
		if err == nil {
			err = errors.New("tool call failed")
		}
		return ToolCallEnded{
			CallID: se.CallID,
			Tool:   toolByCall[se.CallID],
			Err:    err,
		}, false
	case event.KindRunCompleted:
		return Done{TerminalKind: se.Kind, FinalText: se.Text}, true
	case event.KindRunFailed:
		err := se.Err
		if err == nil {
			err = errors.New("run failed")
		}
		return Done{TerminalKind: se.Kind, Err: err}, true
	case event.KindRunCancelled:
		return Done{TerminalKind: se.Kind, Err: context.Canceled}, true
	}
	return nil, false
}
