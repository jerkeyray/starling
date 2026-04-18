package event_test

import (
	"reflect"
	"testing"

	"github.com/jerkeyray/starling/event"
)

func TestKind_String(t *testing.T) {
	cases := []struct {
		k    event.Kind
		want string
	}{
		{event.KindRunStarted, "RunStarted"},
		{event.KindUserMessageAppended, "UserMessageAppended"},
		{event.KindTurnStarted, "TurnStarted"},
		{event.KindReasoningEmitted, "ReasoningEmitted"},
		{event.KindAssistantMessageCompleted, "AssistantMessageCompleted"},
		{event.KindToolCallScheduled, "ToolCallScheduled"},
		{event.KindToolCallCompleted, "ToolCallCompleted"},
		{event.KindToolCallFailed, "ToolCallFailed"},
		{event.KindSideEffectRecorded, "SideEffectRecorded"},
		{event.KindBudgetExceeded, "BudgetExceeded"},
		{event.KindContextTruncated, "ContextTruncated"},
		{event.KindRunCompleted, "RunCompleted"},
		{event.KindRunFailed, "RunFailed"},
		{event.KindRunCancelled, "RunCancelled"},
		{event.Kind(99), "Kind(99)"},
		{event.Kind(15), "Kind(15)"}, // reserved slot, not yet named
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("Kind(%d).String() = %q, want %q", uint8(c.k), got, c.want)
		}
	}
}

func TestKind_IsTerminal(t *testing.T) {
	terminal := map[event.Kind]bool{
		event.KindRunCompleted: true,
		event.KindRunFailed:    true,
		event.KindRunCancelled: true,
	}
	for k := event.Kind(1); k <= event.Kind(14); k++ {
		if got, want := k.IsTerminal(), terminal[k]; got != want {
			t.Errorf("Kind(%s).IsTerminal() = %v, want %v", k, got, want)
		}
	}
	// Unknown kind: not terminal.
	if event.Kind(99).IsTerminal() {
		t.Errorf("Kind(99).IsTerminal() should be false")
	}
}

func TestEvent_RoundTrip(t *testing.T) {
	payload, err := event.EncodePayload(event.UserMessageAppended{Content: "hello"})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	orig := event.Event{
		RunID:     "01HXYZ",
		Seq:       42,
		PrevHash:  []byte{0xde, 0xad, 0xbe, 0xef},
		Timestamp: 1_700_000_000_000_000_000,
		Kind:      event.KindUserMessageAppended,
		Payload:   payload,
	}

	data, err := event.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := event.Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("envelope round-trip mismatch:\nwant=%+v\ngot =%+v", orig, got)
	}

	// And the typed accessor round-trip.
	um, err := got.AsUserMessageAppended()
	if err != nil {
		t.Fatalf("AsUserMessageAppended: %v", err)
	}
	if um.Content != "hello" {
		t.Fatalf("content = %q, want %q", um.Content, "hello")
	}
}
