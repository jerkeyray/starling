package event_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerkeyray/starling/event"
)

// TestToJSON_AllKinds round-trips one event of every Kind through
// EncodePayload → Event → ToJSON → json.Unmarshal back into a map,
// asserting the JSON shape uses snake_case (matching the cbor wire
// format) and not Go field names.
func TestToJSON_AllKinds(t *testing.T) {
	cases := []struct {
		name      string
		kind      event.Kind
		payload   any
		fieldHint string // a snake_case field that must appear in the JSON output
	}{
		{"RunStarted", event.KindRunStarted, event.RunStarted{Goal: "g", ProviderID: "p"}, "provider_id"},
		{"UserMessageAppended", event.KindUserMessageAppended, event.UserMessageAppended{Content: "hi"}, "content"},
		{"TurnStarted", event.KindTurnStarted, event.TurnStarted{TurnID: "t1", InputTokens: 5}, "input_tokens"},
		{"ReasoningEmitted", event.KindReasoningEmitted, event.ReasoningEmitted{TurnID: "t1", Content: "r"}, "turn_id"},
		{"AssistantMessageCompleted", event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "t1", Text: "x", StopReason: "stop"}, "stop_reason"},
		{"ToolCallScheduled", event.KindToolCallScheduled, event.ToolCallScheduled{CallID: "c1", ToolName: "fetch", Attempt: 1}, "call_id"},
		{"ToolCallCompleted", event.KindToolCallCompleted, event.ToolCallCompleted{CallID: "c1", DurationMs: 10, Attempt: 1}, "duration_ms"},
		{"ToolCallFailed", event.KindToolCallFailed, event.ToolCallFailed{CallID: "c1", Error: "boom", ErrorType: "panic", Attempt: 1}, "error_type"},
		{"SideEffectRecorded", event.KindSideEffectRecorded, event.SideEffectRecorded{Name: "now"}, "name"},
		{"BudgetExceeded", event.KindBudgetExceeded, event.BudgetExceeded{Limit: "usd", Cap: 0.5, Actual: 0.6, Where: "mid_stream"}, "limit"},
		{"ContextTruncated", event.KindContextTruncated, event.ContextTruncated{Strategy: "drop_oldest", TokensBefore: 100, TokensAfter: 50}, "tokens_before"},
		{"RunCompleted", event.KindRunCompleted, event.RunCompleted{FinalText: "done", TurnCount: 2}, "final_text"},
		{"RunFailed", event.KindRunFailed, event.RunFailed{Error: "boom", ErrorType: "provider"}, "error_type"},
		{"RunCancelled", event.KindRunCancelled, event.RunCancelled{Reason: "context_canceled"}, "reason"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := event.EncodePayload(tc.payload)
			if err != nil {
				t.Fatalf("EncodePayload: %v", err)
			}
			ev := event.Event{Kind: tc.kind, Payload: encoded}
			out, err := event.ToJSON(ev)
			if err != nil {
				t.Fatalf("ToJSON: %v", err)
			}
			if !strings.Contains(string(out), `"`+tc.fieldHint+`"`) {
				t.Fatalf("ToJSON output missing snake_case field %q: %s", tc.fieldHint, out)
			}
			// Must be valid JSON.
			var generic map[string]any
			if err := json.Unmarshal(out, &generic); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, out)
			}
		})
	}
}

// TestToJSON_UnknownKind asserts the dispatcher returns an error
// rather than silently emitting "{}" — operators should know when an
// inspector hits an event kind it can't decode.
func TestToJSON_UnknownKind(t *testing.T) {
	ev := event.Event{Kind: event.Kind(99)}
	if _, err := event.ToJSON(ev); err == nil {
		t.Fatal("ToJSON for unknown kind: want error, got nil")
	}
}
