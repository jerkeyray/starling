package starling_test

import (
	"context"
	"testing"
	"time"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/tool"
)

// drainStepEvents reads all StepEvents from ch until close or timeout.
// Fails the test if the channel doesn't close within d. Mirrors the
// shape of replay/stream_test.go's drain helper.
func drainStepEvents(t *testing.T, ch <-chan starling.StepEvent, d time.Duration) []starling.StepEvent {
	t.Helper()
	var out []starling.StepEvent
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case se, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, se)
		case <-timer.C:
			t.Fatalf("drainStepEvents: channel did not close within %s", d)
			return out
		}
	}
}

func stepKinds(evs []starling.StepEvent) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, se := range evs {
		out[i] = se.Kind
	}
	return out
}

func TestStream_HappyPath(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "hello"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 4, OutputTokens: 2}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	runID, ch, err := a.Stream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if runID == "" {
		t.Fatal("runID empty")
	}

	evs := drainStepEvents(t, ch, 2*time.Second)
	got := stepKinds(evs)
	want := []event.Kind{
		event.KindRunStarted,
		event.KindTurnStarted,
		event.KindAssistantMessageCompleted,
		event.KindRunCompleted,
	}
	if !kindsEq(got, want) {
		t.Fatalf("kinds = %v\n want %v", got, want)
	}

	// Projection checks: RunStarted.Text == goal; AMC.Text == "hello";
	// RunCompleted.Text == final text.
	if evs[0].Text != "hi" {
		t.Errorf("RunStarted.Text = %q, want %q", evs[0].Text, "hi")
	}
	if evs[1].TurnID == "" {
		t.Errorf("TurnStarted.TurnID empty")
	}
	if evs[2].Text != "hello" {
		t.Errorf("AMC.Text = %q, want %q", evs[2].Text, "hello")
	}
	if evs[3].Text != "hello" {
		t.Errorf("RunCompleted.Text = %q, want %q", evs[3].Text, "hello")
	}

	// runID returned by Stream matches the first event's RunID.
	if evs[0].Raw.RunID != runID {
		t.Errorf("RunStarted.RunID = %q, runID = %q", evs[0].Raw.RunID, runID)
	}
}

func TestStream_Tools(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 2, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
		{
			{Kind: provider.ChunkText, Text: "done"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 3, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	_, ch, err := a.Stream(context.Background(), "go")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	evs := drainStepEvents(t, ch, 2*time.Second)

	// Find the ToolCallScheduled / Completed events by kind.
	var sched, done *starling.StepEvent
	for i := range evs {
		switch evs[i].Kind {
		case event.KindToolCallScheduled:
			sched = &evs[i]
		case event.KindToolCallCompleted:
			done = &evs[i]
		}
	}
	if sched == nil {
		t.Fatal("no ToolCallScheduled StepEvent")
	}
	if done == nil {
		t.Fatal("no ToolCallCompleted StepEvent")
	}
	if sched.Tool != "echo" || sched.CallID != "c1" || sched.TurnID == "" {
		t.Errorf("sched projection = %+v, want Tool=echo CallID=c1 TurnID!=\"\"", *sched)
	}
	if done.CallID != "c1" {
		t.Errorf("completed.CallID = %q, want c1", done.CallID)
	}
}

func TestStream_ClosesOnCtxCancel(t *testing.T) {
	// blockingProvider (defined in agent_test.go) blocks on Next until
	// ctx cancels, so we can deterministically exercise cancel mid-run.
	p := blockingProvider{}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, ch, err := a.Stream(ctx, "hang")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Let the run emit RunStarted.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no event received before cancel")
	}
	cancel()
	// Channel must close within a couple of seconds.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel did not close after ctx cancel")
		}
	}
}

func TestStream_ClosesOnRunFailed(t *testing.T) {
	// Empty scripts ⇒ cannedProvider.Stream returns "exhausted" error
	// on the first call, which surfaces as a ProviderError → RunFailed.
	p := &cannedProvider{}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	_, ch, err := a.Stream(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := drainStepEvents(t, ch, 2*time.Second)
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	last := evs[len(evs)-1]
	if last.Kind != event.KindRunFailed {
		t.Fatalf("last kind = %s, want RunFailed", last.Kind)
	}
	if last.Err == nil {
		t.Errorf("RunFailed StepEvent.Err is nil")
	}
}

func TestStream_ReturnsSetupError(t *testing.T) {
	// Provider nil ⇒ validate() fails synchronously.
	a := &starling.Agent{
		Log:    eventlog.NewInMemory(),
		Config: starling.Config{Model: "gpt-4o-mini"},
	}
	runID, ch, err := a.Stream(context.Background(), "x")
	if err == nil {
		t.Fatal("expected validate error")
	}
	if runID != "" {
		t.Errorf("runID = %q on error path, want empty", runID)
	}
	if ch != nil {
		t.Errorf("ch non-nil on error path")
	}
}

func TestStream_RunIDMatches(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "hi"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider:  p,
		Log:       log,
		Config:    starling.Config{Model: "gpt-4o-mini", MaxTurns: 2},
		Namespace: "ns",
	}
	runID, ch, err := a.Stream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Drain to ensure events flow before we assert.
	evs := drainStepEvents(t, ch, 2*time.Second)
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	if got := evs[0].Raw.RunID; got != runID {
		t.Errorf("first event RunID = %q, want %q", got, runID)
	}
	// Namespace prefix applied.
	if len(runID) < 3 || runID[:3] != "ns/" {
		t.Errorf("runID = %q, want ns/ prefix", runID)
	}
}

// blockingProvider / blockingStream live in agent_test.go.
