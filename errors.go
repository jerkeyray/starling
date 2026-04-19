package starling

import (
	"errors"
	"fmt"

	"github.com/jerkeyray/starling/eventlog"
)

// Sentinel errors surfaced by Agent.Run. Tests and callers route on
// these with errors.Is; the agent loop converts matching errors into
// the appropriate terminal event (RunFailed vs RunCancelled) before
// propagating.
var (
	// ErrBudgetExceeded is returned when a budget cap trips. The
	// matching BudgetExceeded event precedes the terminal RunFailed
	// in the log.
	ErrBudgetExceeded = errors.New("starling: budget exceeded")

	// ErrMaxTurnsExceeded is returned when the loop reaches Config.MaxTurns
	// without the model producing a final answer.
	ErrMaxTurnsExceeded = errors.New("starling: max turns exceeded")

	// ErrNonDeterminism is reserved for M2's replay verifier.
	ErrNonDeterminism = errors.New("starling: non-determinism detected during replay")

	// ErrRunNotFound is reserved for Resume/Replay.
	ErrRunNotFound = errors.New("starling: run not found in log")

	// ErrLogCorrupt wraps every eventlog.Validate failure. Aliased
	// from the eventlog package so callers that never import eventlog
	// directly can still route on it.
	ErrLogCorrupt = eventlog.ErrLogCorrupt
)

// ToolError wraps an error returned by a tool invocation with the
// tool's name and the CallID of the offending call. Used when the
// agent loop bails because of an unrecoverable tool failure.
type ToolError struct {
	Name   string
	CallID string
	Err    error
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("starling: tool %q (call %s): %v", e.Name, e.CallID, e.Err)
}

func (e *ToolError) Unwrap() error { return e.Err }

// ProviderError wraps an error from the LLM provider (stream open
// failure, mid-stream error). Provider is the provider ID
// (e.g. "openai"); Code carries an HTTP status if the adapter
// surfaced one, 0 otherwise.
type ProviderError struct {
	Provider string
	Code     int
	Err      error
}

func (e *ProviderError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("starling: provider %q (status %d): %v", e.Provider, e.Code, e.Err)
	}
	return fmt.Sprintf("starling: provider %q: %v", e.Provider, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }
