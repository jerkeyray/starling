package event

import (
	"fmt"

	"github.com/jerkeyray/starling/internal/cborenc"
)

// Marshal encodes ev into canonical CBOR. The output is deterministic so
// the hash chain and Merkle commitment are reproducible.
func Marshal(ev Event) ([]byte, error) {
	return cborenc.Marshal(ev)
}

// Unmarshal decodes bytes produced by Marshal back into an Event. Payload
// stays as raw CBOR; use the AsXxx accessors to decode it.
func Unmarshal(data []byte) (Event, error) {
	var ev Event
	if err := cborenc.Unmarshal(data, &ev); err != nil {
		return Event{}, fmt.Errorf("event: unmarshal envelope: %w", err)
	}
	return ev, nil
}

// EncodePayload marshals a typed payload to canonical CBOR for assignment
// into Event.Payload.
func EncodePayload[T any](p T) (cborenc.RawMessage, error) {
	b, err := cborenc.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("event: encode payload: %w", err)
	}
	return cborenc.RawMessage(b), nil
}

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

// The AsXxx accessors below decode e.Payload into the matching payload
// struct. Each errors if e.Kind doesn't match — never returns a zero-valued
// payload silently.

func (e Event) AsRunStarted() (RunStarted, error) {
	return asPayload[RunStarted](e, KindRunStarted)
}

func (e Event) AsUserMessageAppended() (UserMessageAppended, error) {
	return asPayload[UserMessageAppended](e, KindUserMessageAppended)
}

func (e Event) AsTurnStarted() (TurnStarted, error) {
	return asPayload[TurnStarted](e, KindTurnStarted)
}

func (e Event) AsReasoningEmitted() (ReasoningEmitted, error) {
	return asPayload[ReasoningEmitted](e, KindReasoningEmitted)
}

func (e Event) AsAssistantMessageCompleted() (AssistantMessageCompleted, error) {
	return asPayload[AssistantMessageCompleted](e, KindAssistantMessageCompleted)
}

func (e Event) AsToolCallScheduled() (ToolCallScheduled, error) {
	return asPayload[ToolCallScheduled](e, KindToolCallScheduled)
}

func (e Event) AsToolCallCompleted() (ToolCallCompleted, error) {
	return asPayload[ToolCallCompleted](e, KindToolCallCompleted)
}

func (e Event) AsToolCallFailed() (ToolCallFailed, error) {
	return asPayload[ToolCallFailed](e, KindToolCallFailed)
}

func (e Event) AsSideEffectRecorded() (SideEffectRecorded, error) {
	return asPayload[SideEffectRecorded](e, KindSideEffectRecorded)
}

func (e Event) AsBudgetExceeded() (BudgetExceeded, error) {
	return asPayload[BudgetExceeded](e, KindBudgetExceeded)
}

func (e Event) AsContextTruncated() (ContextTruncated, error) {
	return asPayload[ContextTruncated](e, KindContextTruncated)
}

func (e Event) AsRunCompleted() (RunCompleted, error) {
	return asPayload[RunCompleted](e, KindRunCompleted)
}

func (e Event) AsRunFailed() (RunFailed, error) {
	return asPayload[RunFailed](e, KindRunFailed)
}

func (e Event) AsRunCancelled() (RunCancelled, error) {
	return asPayload[RunCancelled](e, KindRunCancelled)
}

func (e Event) AsRunResumed() (RunResumed, error) {
	return asPayload[RunResumed](e, KindRunResumed)
}
