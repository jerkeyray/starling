package starling

import (
	"time"

	"github.com/jerkeyray/starling/event"
)

// RunResult is the user-facing summary of a completed agent run.
// Populated from the events the Run emitted into the log — the same
// values are recoverable by replaying the log, so RunResult is a
// convenience, not a source of truth.
type RunResult struct {
	RunID         string
	FinalText     string
	TurnCount     int
	ToolCallCount int
	TotalCostUSD  float64
	InputTokens   int64
	OutputTokens  int64
	CacheStats    CacheStats
	Duration      time.Duration
	TerminalKind  event.Kind // RunCompleted | RunFailed | RunCancelled
	MerkleRoot    []byte
}

// CacheStats summarizes prompt-cache activity over the run, aggregated
// from per-turn AssistantMessageCompleted events. Only Anthropic and
// providers that surface cache token counts populate non-zero values;
// for others CacheStats is the zero value.
//
// Semantics:
//   - ReadTokens / CreateTokens: sums of CacheReadTokens and
//     CacheCreateTokens across every turn.
//   - Hits: number of turns whose CacheReadTokens was greater than 0.
//   - Misses: number of turns that consumed input but did not read
//     any cached prefix (CacheReadTokens == 0 && InputTokens > 0).
type CacheStats struct {
	Hits         int
	Misses       int
	ReadTokens   int64
	CreateTokens int64
}

// StepEvent is the user-facing projection of one event, used by the
// future streaming API (Agent.Stream). Narrower than event.Event so
// consumers don't have to decode payloads themselves for common cases.
type StepEvent struct {
	Kind   event.Kind
	TurnID string
	CallID string
	Text   string      // assistant text, reasoning content, or tool result
	Tool   string      // for tool call events
	Err    error       // set on Failed kinds
	Raw    event.Event // full envelope for consumers that want everything
}
