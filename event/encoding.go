package event

import (
	"fmt"

	"github.com/jerkeyray/starling/internal/cborenc"
)

// Marshal encodes ev into canonical CBOR. Deterministic: two runs that
// emit the same logical event produce byte-identical output, which is
// what the hash chain (and Merkle commitment) relies on.
func Marshal(ev Event) ([]byte, error) {
	return cborenc.Marshal(ev)
}

// Unmarshal decodes canonical-CBOR bytes produced by Marshal back into
// an Event envelope. The Payload field is left as raw CBOR; use the
// AsXxx accessors to decode it into the typed payload for the event's
// Kind.
func Unmarshal(data []byte) (Event, error) {
	var ev Event
	if err := cborenc.Unmarshal(data, &ev); err != nil {
		return Event{}, fmt.Errorf("event: unmarshal envelope: %w", err)
	}
	return ev, nil
}

// EncodePayload marshals a typed payload struct to canonical CBOR bytes
// suitable for assigning into Event.Payload.
func EncodePayload[T any](p T) (cborenc.RawMessage, error) {
	b, err := cborenc.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("event: encode payload: %w", err)
	}
	return cborenc.RawMessage(b), nil
}

// asPayload checks that e.Kind matches want, then decodes e.Payload into T.
func asPayload[T any](e Event, want Kind) (T, error) {
	var zero T
	if e.Kind != want {
		return zero, fmt.Errorf("event: expected kind %s, got %s", want, e.Kind)
	}
	if err := cborenc.Unmarshal(e.Payload, &zero); err != nil {
		return zero, fmt.Errorf("event: decode %s payload: %w", want, err)
	}
	return zero, nil
}

// Every AsXxx accessor below asserts e.Kind matches the expected kind,
// then decodes e.Payload into the matching payload struct. Wrong-kind
// returns an error instead of a zero-valued payload so callers can't
// silently read through the wrong type.

// AsRunStarted decodes e into a RunStarted payload. Returns an error if
// e.Kind is not KindRunStarted.
func (e Event) AsRunStarted() (RunStarted, error) {
	return asPayload[RunStarted](e, KindRunStarted)
}

// AsUserMessageAppended decodes e into a UserMessageAppended payload.
// Returns an error if e.Kind is not KindUserMessageAppended.
func (e Event) AsUserMessageAppended() (UserMessageAppended, error) {
	return asPayload[UserMessageAppended](e, KindUserMessageAppended)
}

// AsTurnStarted decodes e into a TurnStarted payload. Returns an error
// if e.Kind is not KindTurnStarted.
func (e Event) AsTurnStarted() (TurnStarted, error) {
	return asPayload[TurnStarted](e, KindTurnStarted)
}

// AsReasoningEmitted decodes e into a ReasoningEmitted payload. Returns
// an error if e.Kind is not KindReasoningEmitted.
func (e Event) AsReasoningEmitted() (ReasoningEmitted, error) {
	return asPayload[ReasoningEmitted](e, KindReasoningEmitted)
}

// AsAssistantMessageCompleted decodes e into an AssistantMessageCompleted
// payload. Returns an error if e.Kind is not KindAssistantMessageCompleted.
func (e Event) AsAssistantMessageCompleted() (AssistantMessageCompleted, error) {
	return asPayload[AssistantMessageCompleted](e, KindAssistantMessageCompleted)
}

// AsToolCallScheduled decodes e into a ToolCallScheduled payload.
// Returns an error if e.Kind is not KindToolCallScheduled.
func (e Event) AsToolCallScheduled() (ToolCallScheduled, error) {
	return asPayload[ToolCallScheduled](e, KindToolCallScheduled)
}

// AsToolCallCompleted decodes e into a ToolCallCompleted payload.
// Returns an error if e.Kind is not KindToolCallCompleted.
func (e Event) AsToolCallCompleted() (ToolCallCompleted, error) {
	return asPayload[ToolCallCompleted](e, KindToolCallCompleted)
}

// AsToolCallFailed decodes e into a ToolCallFailed payload. Returns an
// error if e.Kind is not KindToolCallFailed.
func (e Event) AsToolCallFailed() (ToolCallFailed, error) {
	return asPayload[ToolCallFailed](e, KindToolCallFailed)
}

// AsSideEffectRecorded decodes e into a SideEffectRecorded payload.
// Returns an error if e.Kind is not KindSideEffectRecorded.
func (e Event) AsSideEffectRecorded() (SideEffectRecorded, error) {
	return asPayload[SideEffectRecorded](e, KindSideEffectRecorded)
}

// AsBudgetExceeded decodes e into a BudgetExceeded payload. Returns an
// error if e.Kind is not KindBudgetExceeded.
func (e Event) AsBudgetExceeded() (BudgetExceeded, error) {
	return asPayload[BudgetExceeded](e, KindBudgetExceeded)
}

// AsContextTruncated decodes e into a ContextTruncated payload. Returns
// an error if e.Kind is not KindContextTruncated.
func (e Event) AsContextTruncated() (ContextTruncated, error) {
	return asPayload[ContextTruncated](e, KindContextTruncated)
}

// AsRunCompleted decodes e into a RunCompleted payload. Returns an error
// if e.Kind is not KindRunCompleted.
func (e Event) AsRunCompleted() (RunCompleted, error) {
	return asPayload[RunCompleted](e, KindRunCompleted)
}

// AsRunFailed decodes e into a RunFailed payload. Returns an error if
// e.Kind is not KindRunFailed.
func (e Event) AsRunFailed() (RunFailed, error) {
	return asPayload[RunFailed](e, KindRunFailed)
}

// AsRunCancelled decodes e into a RunCancelled payload. Returns an error
// if e.Kind is not KindRunCancelled.
func (e Event) AsRunCancelled() (RunCancelled, error) {
	return asPayload[RunCancelled](e, KindRunCancelled)
}
