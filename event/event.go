// Package event defines the event types, canonical encoding, and hash-chain
// helpers that form the core of the Starling event log.
package event

import (
	"fmt"

	"github.com/jerkeyray/starling/internal/cborenc"
)

// SchemaVersion is the current event-log wire-format version. Recorded in
// every RunStarted; replayers refuse logs with an unknown version.
const SchemaVersion uint32 = 1

// Event is the envelope shared by every entry in a per-run hash chain.
// Payload holds canonical CBOR for the typed struct named by Kind; use the
// AsXxx accessors to decode it.
type Event struct {
	RunID     string             `cbor:"run_id"`
	Seq       uint64             `cbor:"seq"`
	PrevHash  []byte             `cbor:"prev_hash"`
	Timestamp int64              `cbor:"ts"` // unix nanoseconds
	Kind      Kind               `cbor:"kind"`
	Payload   cborenc.RawMessage `cbor:"payload"`
}

// Kind enumerates the event kinds emitted during an agent run.
type Kind uint8

const (
	KindRunStarted                Kind = 1
	KindUserMessageAppended       Kind = 2
	KindTurnStarted               Kind = 3
	KindReasoningEmitted          Kind = 4
	KindAssistantMessageCompleted Kind = 5
	KindToolCallScheduled         Kind = 6
	KindToolCallCompleted         Kind = 7
	KindToolCallFailed            Kind = 8
	KindSideEffectRecorded        Kind = 9
	KindBudgetExceeded            Kind = 10
	KindContextTruncated          Kind = 11
	KindRunCompleted              Kind = 12
	KindRunFailed                 Kind = 13
	KindRunCancelled              Kind = 14
	KindRunResumed                Kind = 15
)

// String returns the canonical name of k, or "Kind(<n>)" for unknown values
// so log dumps stay readable.
func (k Kind) String() string {
	switch k {
	case KindRunStarted:
		return "RunStarted"
	case KindUserMessageAppended:
		return "UserMessageAppended"
	case KindTurnStarted:
		return "TurnStarted"
	case KindReasoningEmitted:
		return "ReasoningEmitted"
	case KindAssistantMessageCompleted:
		return "AssistantMessageCompleted"
	case KindToolCallScheduled:
		return "ToolCallScheduled"
	case KindToolCallCompleted:
		return "ToolCallCompleted"
	case KindToolCallFailed:
		return "ToolCallFailed"
	case KindSideEffectRecorded:
		return "SideEffectRecorded"
	case KindBudgetExceeded:
		return "BudgetExceeded"
	case KindContextTruncated:
		return "ContextTruncated"
	case KindRunCompleted:
		return "RunCompleted"
	case KindRunFailed:
		return "RunFailed"
	case KindRunCancelled:
		return "RunCancelled"
	case KindRunResumed:
		return "RunResumed"
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// IsTerminal reports whether k ends a run.
func (k Kind) IsTerminal() bool {
	switch k {
	case KindRunCompleted, KindRunFailed, KindRunCancelled:
		return true
	}
	return false
}
