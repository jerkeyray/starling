package event_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/internal/cborenc"
)

func TestEncodePayload_RoundTrip(t *testing.T) {
	in := event.TurnStarted{
		TurnID:      "turn-7",
		PromptHash:  []byte{0x01, 0x02},
		InputTokens: 999,
	}
	raw, err := event.EncodePayload(in)
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	var out event.TurnStarted
	if err := cborenc.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("payload round-trip mismatch:\nwant=%+v\ngot =%+v", in, out)
	}
}

func TestAs_WrongKind(t *testing.T) {
	payload, err := event.EncodePayload(event.RunStarted{SchemaVersion: event.SchemaVersion, Goal: "x"})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	ev := event.Event{Kind: event.KindRunStarted, Payload: payload}

	_, err = ev.AsTurnStarted()
	if err == nil {
		t.Fatalf("expected error calling AsTurnStarted on a RunStarted event")
	}
	if !strings.Contains(err.Error(), "TurnStarted") || !strings.Contains(err.Error(), "RunStarted") {
		t.Fatalf("error should mention both kinds, got: %v", err)
	}
}

// TestAs_SuccessEachKind table-drives every accessor against a matching
// event. Each case builds an Event from a payload and confirms the accessor
// decodes back to the same payload.
func TestAs_SuccessEachKind(t *testing.T) {
	// Required RawMessage fields must be non-nil — CBOR has no distinct
	// representation for "field missing" vs "field = nil", so a nil
	// RawMessage round-trips as CBOR null (0xf6). Real callers always pass
	// canonical CBOR bytes; here we use mustRaw to synthesize the same.
	emptyMap := mustRaw(t, map[string]any{})
	rs := event.RunStarted{SchemaVersion: event.SchemaVersion, Goal: "g", ProviderID: "openai", ModelID: "m", APIVersion: "v1", ParamsHash: []byte{1}, Params: emptyMap, SystemPromptHash: []byte{2}, SystemPrompt: "sp", ToolRegistryHash: []byte{3}, ToolSchemas: []event.ToolSchemaRef{{Name: "t", SchemaHash: []byte{4}}}}
	um := event.UserMessageAppended{Content: "c"}
	ts := event.TurnStarted{TurnID: "t1", PromptHash: []byte{5}, InputTokens: 1}
	re := event.ReasoningEmitted{TurnID: "t1", Content: "r", Sensitive: false}
	am := event.AssistantMessageCompleted{TurnID: "t1", Text: "hi", StopReason: "end", InputTokens: 1, OutputTokens: 1, CostUSD: 0.001, RawResponseHash: []byte{6}}
	tcs := event.ToolCallScheduled{CallID: "c1", TurnID: "t1", ToolName: "fetch", Args: emptyMap, Attempt: 1}
	tcc := event.ToolCallCompleted{CallID: "c1", Result: emptyMap, DurationMs: 1, Attempt: 1}
	tcf := event.ToolCallFailed{CallID: "c1", Error: "e", ErrorType: "timeout", DurationMs: 1, Attempt: 1}
	se := event.SideEffectRecorded{Name: "now", Value: mustRaw(t, int64(1700000000))}
	be := event.BudgetExceeded{Limit: event.LimitUSD, Cap: 0.1, Actual: 0.2, Where: event.WherePreCall}
	ct := event.ContextTruncated{Strategy: "drop_oldest", TokensBefore: 10, TokensAfter: 5, MessagesBefore: 4, MessagesAfter: 2}
	rc := event.RunCompleted{FinalText: "done", TurnCount: 1, ToolCallCount: 0, TotalCostUSD: 0.01, TotalInputTokens: 1, TotalOutputTokens: 1, DurationMs: 10, MerkleRoot: []byte{7}}
	rf := event.RunFailed{Error: "e", ErrorType: "x", MerkleRoot: []byte{8}, DurationMs: 10}
	rx := event.RunCancelled{Reason: "user_cancel", MerkleRoot: []byte{9}, DurationMs: 10}

	check := func(t *testing.T, name string, kind event.Kind, payload any, accessor func(event.Event) (any, error), want any) {
		t.Helper()
		raw, err := cborenc.Marshal(payload)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		ev := event.Event{Kind: kind, Payload: cborenc.RawMessage(raw)}
		got, err := accessor(ev)
		if err != nil {
			t.Fatalf("%s: accessor: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: accessor result mismatch:\nwant=%+v\ngot =%+v", name, want, got)
		}
	}

	// Adapters so each accessor fits the same func signature for the table.
	check(t, "AsRunStarted", event.KindRunStarted, rs,
		func(e event.Event) (any, error) { return e.AsRunStarted() }, rs)
	check(t, "AsUserMessageAppended", event.KindUserMessageAppended, um,
		func(e event.Event) (any, error) { return e.AsUserMessageAppended() }, um)
	check(t, "AsTurnStarted", event.KindTurnStarted, ts,
		func(e event.Event) (any, error) { return e.AsTurnStarted() }, ts)
	check(t, "AsReasoningEmitted", event.KindReasoningEmitted, re,
		func(e event.Event) (any, error) { return e.AsReasoningEmitted() }, re)
	check(t, "AsAssistantMessageCompleted", event.KindAssistantMessageCompleted, am,
		func(e event.Event) (any, error) { return e.AsAssistantMessageCompleted() }, am)
	check(t, "AsToolCallScheduled", event.KindToolCallScheduled, tcs,
		func(e event.Event) (any, error) { return e.AsToolCallScheduled() }, tcs)
	check(t, "AsToolCallCompleted", event.KindToolCallCompleted, tcc,
		func(e event.Event) (any, error) { return e.AsToolCallCompleted() }, tcc)
	check(t, "AsToolCallFailed", event.KindToolCallFailed, tcf,
		func(e event.Event) (any, error) { return e.AsToolCallFailed() }, tcf)
	check(t, "AsSideEffectRecorded", event.KindSideEffectRecorded, se,
		func(e event.Event) (any, error) { return e.AsSideEffectRecorded() }, se)
	check(t, "AsBudgetExceeded", event.KindBudgetExceeded, be,
		func(e event.Event) (any, error) { return e.AsBudgetExceeded() }, be)
	check(t, "AsContextTruncated", event.KindContextTruncated, ct,
		func(e event.Event) (any, error) { return e.AsContextTruncated() }, ct)
	check(t, "AsRunCompleted", event.KindRunCompleted, rc,
		func(e event.Event) (any, error) { return e.AsRunCompleted() }, rc)
	check(t, "AsRunFailed", event.KindRunFailed, rf,
		func(e event.Event) (any, error) { return e.AsRunFailed() }, rf)
	check(t, "AsRunCancelled", event.KindRunCancelled, rx,
		func(e event.Event) (any, error) { return e.AsRunCancelled() }, rx)
}
