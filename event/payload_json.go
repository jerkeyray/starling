package event

import (
	"encoding/json"
	"fmt"
)

// ToJSON decodes ev's payload and re-encodes it as JSON for the
// inspector UI. Field names match the CBOR wire format (snake_case);
// byte slices encode as base64. Errors if Kind is unknown or the
// payload fails to decode.
func ToJSON(ev Event) ([]byte, error) {
	v, err := payloadValue(ev)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

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
