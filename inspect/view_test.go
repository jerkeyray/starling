package inspect

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

func TestStatusOf(t *testing.T) {
	cases := []struct {
		k          event.Kind
		wantLabel  string
		wantClass  string
	}{
		{event.KindRunCompleted, "completed", "ok"},
		{event.KindRunFailed, "failed", "err"},
		{event.KindRunCancelled, "cancelled", "warn"},
		{event.KindRunStarted, "in progress", "muted"},
		{event.KindToolCallScheduled, "in progress", "muted"},
		{event.Kind(0), "in progress", "muted"},
	}
	for _, tc := range cases {
		gotLabel, gotClass := statusOf(tc.k)
		if gotLabel != tc.wantLabel || gotClass != tc.wantClass {
			t.Errorf("statusOf(%v) = (%q,%q), want (%q,%q)",
				tc.k, gotLabel, gotClass, tc.wantLabel, tc.wantClass)
		}
	}
}

func TestKindFamily(t *testing.T) {
	cases := map[event.Kind]string{
		event.KindRunStarted:                "lifecycle",
		event.KindTurnStarted:               "lifecycle",
		event.KindUserMessageAppended:       "message",
		event.KindReasoningEmitted:          "message",
		event.KindAssistantMessageCompleted: "message",
		event.KindToolCallScheduled:         "tool",
		event.KindToolCallCompleted:         "tool",
		event.KindToolCallFailed:            "tool",
		event.KindSideEffectRecorded:        "tool",
		event.KindBudgetExceeded:            "budget",
		event.KindContextTruncated:          "budget",
		event.KindRunCompleted:              "terminal",
		event.KindRunFailed:                 "terminal",
		event.KindRunCancelled:              "terminal",
		event.Kind(99):                      "unknown",
	}
	for k, want := range cases {
		if got := kindFamily(k); got != want {
			t.Errorf("kindFamily(%v) = %q, want %q", k, got, want)
		}
	}
}

func TestFilterByStatus(t *testing.T) {
	rows := []runRow{
		{RunID: "a", StatusLabel: "completed"},
		{RunID: "b", StatusLabel: "failed"},
		{RunID: "c", StatusLabel: "completed"},
		{RunID: "d", StatusLabel: "in progress"},
	}
	if got := filterByStatus(rows, ""); len(got) != 4 {
		t.Errorf("empty filter dropped rows: got %d, want 4", len(got))
	}
	got := filterByStatus(rows, "completed")
	if len(got) != 2 || got[0].RunID != "a" || got[1].RunID != "c" {
		t.Errorf("status=completed = %+v, want a,c", got)
	}
	if got := filterByStatus(rows, "nonsense"); len(got) != 0 {
		t.Errorf("unknown status returned %d rows, want 0", len(got))
	}
}

func TestRowsFromSummaries(t *testing.T) {
	now := time.Date(2026, 4, 20, 10, 30, 0, 0, time.UTC)
	in := []eventlog.RunSummary{
		{RunID: "r1", StartedAt: now, LastSeq: 7, TerminalKind: event.KindRunFailed},
	}
	out := rowsFromSummaries(in)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	r := out[0]
	if r.RunID != "r1" || r.EventCount != 7 || r.StatusLabel != "failed" || r.StatusClass != "err" {
		t.Errorf("row = %+v", r)
	}
	if r.StartedISO != "2026-04-20T10:30:00Z" {
		t.Errorf("StartedISO = %q", r.StartedISO)
	}
}

func TestSummarize(t *testing.T) {
	mk := func(kind event.Kind, payload any) event.Event {
		t.Helper()
		b, err := event.EncodePayload(payload)
		if err != nil {
			t.Fatalf("EncodePayload: %v", err)
		}
		return event.Event{Kind: kind, Payload: b}
	}
	cases := []struct {
		name        string
		ev          event.Event
		wantPrefix  string
		wantCallID  string
	}{
		{"RunStarted", mk(event.KindRunStarted, event.RunStarted{ModelID: "gpt-4o-mini"}), "model=gpt-4o-mini", ""},
		{"ToolCallScheduled", mk(event.KindToolCallScheduled, event.ToolCallScheduled{CallID: "c1", ToolName: "fetch"}), "tool=fetch call=c1", "c1"},
		{"ToolCallCompleted", mk(event.KindToolCallCompleted, event.ToolCallCompleted{CallID: "c1", DurationMs: 42}), "call=c1 ok 42ms", "c1"},
		{"ToolCallFailed", mk(event.KindToolCallFailed, event.ToolCallFailed{CallID: "c1", ErrorType: "timeout"}), "call=c1 err=timeout", "c1"},
		{"BudgetExceeded", mk(event.KindBudgetExceeded, event.BudgetExceeded{Limit: "usd"}), "budget=usd", ""},
		{"RunCompleted", mk(event.KindRunCompleted, event.RunCompleted{TurnCount: 2, ToolCallCount: 3}), "turns=2 tools=3", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, callID := summarize(tc.ev)
			if got != tc.wantPrefix {
				t.Errorf("summary = %q, want %q", got, tc.wantPrefix)
			}
			if callID != tc.wantCallID {
				t.Errorf("callID = %q, want %q", callID, tc.wantCallID)
			}
		})
	}
}

func TestSummarizeNewlineCollapsed(t *testing.T) {
	b, err := event.EncodePayload(event.UserMessageAppended{Content: "line1\nline2\tafter"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := summarize(event.Event{Kind: event.KindUserMessageAppended, Payload: b})
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("summary should collapse whitespace, got %q", got)
	}
}

func TestShortHex(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"abcdef":       "abcdef",
		"abcdefabcdef": "abcdefabcdef",
		"abcdefabcdef0": "abcdefabcdef…",
	}
	for in, want := range cases {
		if got := shortHex(in); got != want {
			t.Errorf("shortHex(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidationFromError(t *testing.T) {
	if v := validationFromError(nil); !v.OK || v.Class != "ok" || v.Reason != "" {
		t.Errorf("ok case: %+v", v)
	}
	wrapped := fmt.Errorf("%w: prev-hash mismatch at index 2", eventlog.ErrLogCorrupt)
	v := validationFromError(wrapped)
	if v.OK || v.Class != "err" || v.Label != "chain invalid" {
		t.Errorf("err case: %+v", v)
	}
	if v.Reason != "prev-hash mismatch at index 2" {
		t.Errorf("Reason = %q, want stripped of wrapper prefix", v.Reason)
	}
	// Non-ErrLogCorrupt errors should still surface as invalid, with raw message.
	plain := errors.New("io: read failed")
	v2 := validationFromError(plain)
	if v2.OK || v2.Reason != "io: read failed" {
		t.Errorf("plain err: %+v", v2)
	}
}

func TestRowsFromEventsBuildsURL(t *testing.T) {
	b, err := event.EncodePayload(event.RunStarted{ModelID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	ev := event.Event{RunID: "r1", Seq: 7, Kind: event.KindRunStarted, Payload: b, Timestamp: time.Now().UnixNano()}
	rows := rowsFromEvents("r1", []event.Event{ev})
	if len(rows) != 1 {
		t.Fatalf("len = %d", len(rows))
	}
	got := rows[0]
	if got.SeqLabel != "#0007" {
		t.Errorf("SeqLabel = %q", got.SeqLabel)
	}
	if got.DetailURL != "/run/r1/event/7" {
		t.Errorf("DetailURL = %q", got.DetailURL)
	}
	if got.Family != "lifecycle" {
		t.Errorf("Family = %q", got.Family)
	}
}
