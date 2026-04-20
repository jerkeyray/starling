package event

import (
	"encoding/json"
	"fmt"
)

// ToJSON returns ev's payload decoded into its typed struct and re-
// encoded as JSON. It is the wire format the inspector UI (and any
// other JSON-consuming tooling) reads.
//
// The returned bytes use the same field names as the CBOR wire format
// (snake_case) — every payload struct in this package carries
// identical `cbor` and `json` struct tags so an event renders the
// same on the wire and on screen.
//
// Byte-slice fields (e.g. PrevHash, ParamsHash, RawMessage) JSON-
// encode as base64. Inspector-side code that wants to drill into a
// nested cborenc.RawMessage (Args, Params, Value) can decode it
// separately via cborenc.Unmarshal into a generic map[string]any.
//
// ToJSON is a free function rather than a method on Event because
// the JSON projection is an inspector concern, not a core event-
// envelope concern: keeping it as a function makes future
// deprecation (and inspector-local replacement) cheap.
//
// Returns an error if ev.Kind is unknown or if the payload fails to
// decode against its expected type — the latter usually means the
// log was written by a newer Starling version with a richer payload.
func ToJSON(ev Event) ([]byte, error) {
	v, err := payloadValue(ev)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// payloadValue dispatches on Kind, calling the matching AsXxx
// accessor and returning the typed payload as an any.
func payloadValue(e Event) (any, error) {
	switch e.Kind {
	case KindRunStarted:
		return e.AsRunStarted()
	case KindUserMessageAppended:
		return e.AsUserMessageAppended()
	case KindTurnStarted:
		return e.AsTurnStarted()
	case KindReasoningEmitted:
		return e.AsReasoningEmitted()
	case KindAssistantMessageCompleted:
		return e.AsAssistantMessageCompleted()
	case KindToolCallScheduled:
		return e.AsToolCallScheduled()
	case KindToolCallCompleted:
		return e.AsToolCallCompleted()
	case KindToolCallFailed:
		return e.AsToolCallFailed()
	case KindSideEffectRecorded:
		return e.AsSideEffectRecorded()
	case KindBudgetExceeded:
		return e.AsBudgetExceeded()
	case KindContextTruncated:
		return e.AsContextTruncated()
	case KindRunCompleted:
		return e.AsRunCompleted()
	case KindRunFailed:
		return e.AsRunFailed()
	case KindRunCancelled:
		return e.AsRunCancelled()
	case KindRunResumed:
		return e.AsRunResumed()
	}
	return nil, fmt.Errorf("event: ToJSON: unknown kind %s", e.Kind)
}
