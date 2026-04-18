// Package event defines the event types, canonical encoding, and hash-chain
// helpers that form the core of the Starling event log.
package event

import (
	"fmt"

	"github.com/jerkeyray/starling/internal/cborenc"
)

// SchemaVersion is the current event-log wire-format version. It is recorded
// in every RunStarted event; a replayer refuses logs whose schema version it
// does not understand.
const SchemaVersion uint32 = 1

// Event is the envelope every entry in a per-run hash chain shares.
//
// The envelope is intentionally small and payload-agnostic. The Payload field
// holds canonical CBOR bytes of a typed payload struct (see types.go); the
// matching Kind tells callers which type to decode into. Accessor methods on
// Event (AsRunStarted, AsToolCallCompleted, …) combine that check and decode.
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
	// 15 reserved for KindTurnFailed.
)

// String returns the canonical name of k. Unknown kinds render as
// "Kind(<n>)" so log dumps remain readable.
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
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// IsTerminal reports whether k ends a run (RunCompleted, RunFailed, RunCancelled).
func (k Kind) IsTerminal() bool {
	switch k {
	case KindRunCompleted, KindRunFailed, KindRunCancelled:
		return true
	}
	return false
}
