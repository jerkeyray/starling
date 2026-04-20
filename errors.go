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

	// ErrRunNotFound is returned by Resume when the requested runID has
	// no events in the log.
	ErrRunNotFound = errors.New("starling: run not found in log")

	// ErrRunAlreadyTerminal is returned by Resume when the run's last
	// event is a terminal kind (RunCompleted/RunFailed/RunCancelled).
	// Resuming a terminated run is not supported — the terminal event
	// commits a Merkle root over every event before it, and appending
	// past that point would invalidate the commitment.
	ErrRunAlreadyTerminal = errors.New("starling: run already terminal")

	// ErrSchemaVersionMismatch is returned by Resume when the run's
	// RunStarted event records a schema version this binary does not
	// understand.
	ErrSchemaVersionMismatch = errors.New("starling: event schema version mismatch")

	// ErrPartialToolCall is returned by Resume when the run's tail
	// contains a ToolCallScheduled event without a matching
	// ToolCallCompleted/ToolCallFailed, and WithReissueTools(false) was
	// passed. It signals that the resuming process would otherwise have
	// to re-issue a tool call of unknown idempotency.
	ErrPartialToolCall = errors.New("starling: run has a partial tool call; pass WithReissueTools(true) to reissue")

	// ErrRunInUse is returned by Resume when its first Append onto the
	// existing chain is rejected because another writer has advanced
	// the tail under us. Indicates two processes are racing to resume
	// the same run — the loser bails cleanly rather than risk chain
	// corruption.
	ErrRunInUse = errors.New("starling: run is being appended by another writer")

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

// Error implements the error interface, formatting the tool name,
// CallID, and underlying error.
func (e *ToolError) Error() string {
	return fmt.Sprintf("starling: tool %q (call %s): %v", e.Name, e.CallID, e.Err)
}

// Unwrap returns the underlying tool error so callers can route on it
// with errors.Is / errors.As.
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

// Error implements the error interface, including the provider ID and
// HTTP status code (when non-zero).
func (e *ProviderError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("starling: provider %q (status %d): %v", e.Provider, e.Code, e.Err)
	}
	return fmt.Sprintf("starling: provider %q: %v", e.Provider, e.Err)
}

// Unwrap returns the underlying provider error so callers can route on
// it with errors.Is / errors.As.
func (e *ProviderError) Unwrap() error { return e.Err }
