package event_test

import (
	"reflect"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/internal/cborenc"
)

// Sample RawMessage values used in a few fixtures. Must themselves be
// canonical CBOR bytes (produced by cborenc.Marshal); we don't hand-craft
// bytes here because that drifts from the encoder.
func mustRaw(t *testing.T, v any) cborenc.RawMessage {
	t.Helper()
	b, err := cborenc.Marshal(v)
	if err != nil {
		t.Fatalf("mustRaw: %v", err)
	}
	return cborenc.RawMessage(b)
}

// TestPayload_RoundTrip table-drives every payload type through canonical
// marshal/unmarshal. Each entry constructs a fully-populated value (every
// non-optional field non-zero) so tag typos and wrong Go types surface here.
func TestPayload_RoundTrip(t *testing.T) {
	t.Run("RunStarted", func(t *testing.T) {
		want := event.RunStarted{
			SchemaVersion:    event.SchemaVersion,
			Goal:             "summarize release notes",
			ProviderID:       "openai",
			ModelID:          "gpt-4o-mini",
			APIVersion:       "v1",
			ParamsHash:       []byte{0x01, 0x02, 0x03},
			Params:           mustRaw(t, map[string]any{"temperature": 0.7}),
			SystemPromptHash: []byte{0x10, 0x11},
			SystemPrompt:     "You are helpful.",
			ToolRegistryHash: []byte{0x20, 0x21},
			ToolSchemas: []event.ToolSchemaRef{
				{Name: "fetch", SchemaHash: []byte{0xaa}},
				{Name: "read_file", SchemaHash: []byte{0xbb}},
			},
			Budget: &event.BudgetLimits{
				MaxInputTokens:  1000,
				MaxOutputTokens: 2000,
				MaxUSD:          0.10,
				MaxWallClockMs:  30_000,
			},
		}
		assertRoundTrip(t, want)
	})

	t.Run("UserMessageAppended", func(t *testing.T) {
		assertRoundTrip(t, event.UserMessageAppended{Content: "follow-up question"})
	})

	t.Run("TurnStarted", func(t *testing.T) {
		assertRoundTrip(t, event.TurnStarted{
			TurnID:      "turn-1",
			PromptHash:  []byte{0x01, 0x02, 0x03, 0x04},
			InputTokens: 250,
		})
	})

	t.Run("ReasoningEmitted", func(t *testing.T) {
		assertRoundTrip(t, event.ReasoningEmitted{
			TurnID:    "turn-1",
			Content:   "I should call fetch first.",
			Sensitive: true,
		})
	})

	t.Run("AssistantMessageCompleted", func(t *testing.T) {
		assertRoundTrip(t, event.AssistantMessageCompleted{
			TurnID: "turn-1",
			Text:   "Here is the summary.",
			ToolUses: []event.PlannedToolUse{
				{CallID: "call-1", Tool: "fetch", Args: mustRaw(t, map[string]any{"url": "https://example.com"})},
			},
			StopReason:        "end_turn",
			InputTokens:       250,
			OutputTokens:      80,
			CacheReadTokens:   100,
			CacheCreateTokens: 50,
			CostUSD:           0.0042,
			RawResponseHash:   []byte{0xde, 0xad, 0xbe, 0xef},
			ProviderRequestID: "req_abc",
		})
	})

	t.Run("ToolCallScheduled", func(t *testing.T) {
		assertRoundTrip(t, event.ToolCallScheduled{
			CallID:   "call-1",
			TurnID:   "turn-1",
			Tool:     "fetch",
			Args:     mustRaw(t, map[string]any{"url": "https://example.com"}),
			Attempt:  1,
			IdempKey: "idem-xyz",
		})
	})

	t.Run("ToolCallCompleted", func(t *testing.T) {
		assertRoundTrip(t, event.ToolCallCompleted{
			CallID:     "call-1",
			Result:     mustRaw(t, map[string]any{"body": "hello"}),
			DurationMs: 123,
			Attempt:    1,
		})
	})

	t.Run("ToolCallFailed", func(t *testing.T) {
		assertRoundTrip(t, event.ToolCallFailed{
			CallID:     "call-1",
			Error:      "dial tcp: timeout",
			ErrorType:  "timeout",
			DurationMs: 5000,
			Attempt:    2,
		})
	})

	t.Run("SideEffectRecorded", func(t *testing.T) {
		assertRoundTrip(t, event.SideEffectRecorded{
			Name:  "now",
			Value: mustRaw(t, int64(1_700_000_000)),
		})
	})

	t.Run("BudgetExceeded", func(t *testing.T) {
		assertRoundTrip(t, event.BudgetExceeded{
			Limit:         "output_tokens",
			Cap:           2000,
			Actual:        2100,
			Where:         "mid_stream",
			TurnID:        "turn-3",
			CallID:        "",
			PartialText:   "partial output...",
			PartialTokens: 2100,
		})
	})

	t.Run("ContextTruncated", func(t *testing.T) {
		assertRoundTrip(t, event.ContextTruncated{
			Strategy:       "drop_oldest",
			TokensBefore:   12_000,
			TokensAfter:    8_000,
			MessagesBefore: 24,
			MessagesAfter:  16,
		})
	})

	t.Run("RunCompleted", func(t *testing.T) {
		assertRoundTrip(t, event.RunCompleted{
			FinalText:         "Done.",
			TurnCount:         3,
			ToolCallCount:     2,
			TotalCostUSD:      0.015,
			TotalInputTokens:  1200,
			TotalOutputTokens: 400,
			DurationMs:        4500,
			MerkleRoot:        []byte{0xaa, 0xbb, 0xcc, 0xdd},
		})
	})

	t.Run("RunFailed", func(t *testing.T) {
		assertRoundTrip(t, event.RunFailed{
			Error:      "provider returned 500",
			ErrorType:  "provider_error",
			MerkleRoot: []byte{0x01, 0x02, 0x03},
			DurationMs: 3200,
		})
	})

	t.Run("RunCancelled", func(t *testing.T) {
		assertRoundTrip(t, event.RunCancelled{
			Reason:     "context_canceled",
			MerkleRoot: []byte{0x04, 0x05, 0x06},
			DurationMs: 150,
		})
	})
}

// assertRoundTrip marshals v, unmarshals into a fresh T, and asserts deep
// equality with the original. Parameterized on T so the type is preserved
// across the round trip (no interface any shenanigans).
func assertRoundTrip[T any](t *testing.T, v T) {
	t.Helper()
	b, err := cborenc.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got T
	if err := cborenc.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(v, got) {
		t.Fatalf("round-trip mismatch:\nwant=%+v\ngot =%+v", v, got)
	}
}
