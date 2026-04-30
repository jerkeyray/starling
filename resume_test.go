package starling_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/tool"
)

// The resume tests use the same cannedProvider + echoTool from
// agent_test.go. Pattern: Run once with a truncated script so the run
// ends without a terminal event (we manually truncate the log to
// simulate a crash), then Resume with a new script + agent against the
// same log + runID.

func TestResume_UnknownRunID(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: &cannedProvider{},
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini"},
	}
	_, err := a.Resume(context.Background(), "nope", "")
	if !errors.Is(err, starling.ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

func TestResume_AlreadyTerminal(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "done"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 2},
	}
	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The run already ended with RunCompleted — resume must refuse.
	_, err = a.Resume(context.Background(), res.RunID, "")
	if !errors.Is(err, starling.ErrRunAlreadyTerminal) {
		t.Fatalf("err = %v, want ErrRunAlreadyTerminal", err)
	}
}

// truncateAfter chops the log's events for runID back to the first n
// events. Used to simulate a crash mid-run. Only the in-memory backend
// is writable this way via Read+NewInMemory roundtrip.
func seedCrashedRun(t *testing.T, evs []event.Event, stopAfter int) (eventlog.EventLog, string) {
	t.Helper()
	if stopAfter > len(evs) {
		t.Fatalf("stopAfter=%d > len(evs)=%d", stopAfter, len(evs))
	}
	log := eventlog.NewInMemory()
	t.Cleanup(func() { log.Close() })
	for i := 0; i < stopAfter; i++ {
		if err := log.Append(context.Background(), evs[i].RunID, evs[i]); err != nil {
			t.Fatalf("seed append %d: %v", i, err)
		}
	}
	return log, evs[0].RunID
}

// recordThenCrash runs agent a with the given scripts into a fresh log,
// then copies only the first stopAfter events into a new log and
// returns it with the runID. Simulates a process that died before
// writing a terminal event.
func recordThenCrash(t *testing.T, cfg starling.Config, tools []tool.Tool, goal string, scripts [][]provider.StreamChunk, stopAfter int) (eventlog.EventLog, string) {
	t.Helper()
	src := eventlog.NewInMemory()
	t.Cleanup(func() { src.Close() })
	a := &starling.Agent{
		Provider: &cannedProvider{scripts: scripts},
		Tools:    tools,
		Log:      src,
		Config:   cfg,
	}
	res, err := a.Run(context.Background(), goal)
	// Even on errors (e.g., short scripts) the log carries whatever
	// was emitted; we still want to snapshot it.
	_ = err
	evs, rerr := src.Read(context.Background(), res.RunID)
	if rerr != nil {
		t.Fatalf("seed Read: %v", rerr)
	}
	crashLog, runID := seedCrashedRun(t, evs, stopAfter)
	return crashLog, runID
}

func TestResume_AfterAssistantMessage(t *testing.T) {
	// Script 1: assistant plans a tool call → tool runs → turn ends.
	// Script 2 (after crash, used by the resuming agent): produce final text.
	scripts := [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 10, OutputTokens: 5}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
	}
	cfg := starling.Config{Model: "gpt-4o-mini", MaxTurns: 4}

	// Run turn 1 in full (RunStarted, TurnStarted, Assistant, Scheduled,
	// Completed) — that's 5 events; crash before the next turn begins.
	log, runID := recordThenCrash(t, cfg, []tool.Tool{echoTool()}, "do it", scripts, 5)

	// Resume with a fresh script that finalizes.
	resumeProvider := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "all done"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 2, OutputTokens: 2}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	a := &starling.Agent{
		Provider: resumeProvider,
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   cfg,
	}
	res, err := a.Resume(context.Background(), runID, "")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.TerminalKind != event.KindRunCompleted {
		t.Fatalf("TerminalKind = %s, want RunCompleted", res.TerminalKind)
	}
	if res.FinalText != "all done" {
		t.Fatalf("FinalText = %q", res.FinalText)
	}
	// The merged log must validate end-to-end.
	merged, _ := log.Read(context.Background(), runID)
	if err := eventlog.Validate(merged); err != nil {
		t.Fatalf("Validate merged: %v", err)
	}
	// There must be exactly one RunResumed marker.
	var resumedCount int
	for _, ev := range merged {
		if ev.Kind == event.KindRunResumed {
			resumedCount++
		}
	}
	if resumedCount != 1 {
		t.Fatalf("RunResumed count = %d, want 1", resumedCount)
	}
}

func TestResume_WithExtraMessage(t *testing.T) {
	// Run once to completion of turn 1 (5 events: Started, TurnStarted,
	// Assistant+tool, Scheduled, Completed), truncate.
	scripts := [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 10, OutputTokens: 5}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
	}
	cfg := starling.Config{Model: "gpt-4o-mini", MaxTurns: 4}
	log, runID := recordThenCrash(t, cfg, []tool.Tool{echoTool()}, "do it", scripts, 5)

	resumeProvider := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "acknowledged"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 3, OutputTokens: 2}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	a := &starling.Agent{
		Provider: resumeProvider,
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   cfg,
	}
	res, err := a.Resume(context.Background(), runID, "follow up please")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	merged, _ := log.Read(context.Background(), runID)
	if err := eventlog.Validate(merged); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// RunResumed must be immediately followed by UserMessageAppended,
	// then a TurnStarted.
	var saw []event.Kind
	for _, ev := range merged {
		saw = append(saw, ev.Kind)
	}
	// Locate RunResumed.
	idx := -1
	for i, k := range saw {
		if k == event.KindRunResumed {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("no RunResumed event in merged log")
	}
	if saw[idx+1] != event.KindUserMessageAppended {
		t.Fatalf("event after RunResumed = %s, want UserMessageAppended", saw[idx+1])
	}
	if saw[idx+2] != event.KindTurnStarted {
		t.Fatalf("event after UserMessageAppended = %s, want TurnStarted", saw[idx+2])
	}
	if res.FinalText != "acknowledged" {
		t.Fatalf("FinalText = %q", res.FinalText)
	}
}

func TestResume_PendingToolCallReissue(t *testing.T) {
	// Run turn 1 up to ToolCallScheduled (4 events: Started,
	// TurnStarted, Assistant, Scheduled). No Completed → pending.
	scripts := [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 10, OutputTokens: 5}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
	}
	cfg := starling.Config{Model: "gpt-4o-mini", MaxTurns: 4}
	log, runID := recordThenCrash(t, cfg, []tool.Tool{echoTool()}, "do it", scripts, 4)

	resumeProvider := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "fine"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 2, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	a := &starling.Agent{
		Provider: resumeProvider,
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   cfg,
	}
	res, err := a.Resume(context.Background(), runID, "")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.TerminalKind != event.KindRunCompleted {
		t.Fatalf("TerminalKind = %s", res.TerminalKind)
	}

	merged, _ := log.Read(context.Background(), runID)
	if err := eventlog.Validate(merged); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Look at the RunResumed payload.
	var resumed event.RunResumed
	for _, ev := range merged {
		if ev.Kind == event.KindRunResumed {
			rr, derr := ev.AsRunResumed()
			if derr != nil {
				t.Fatalf("decode RunResumed: %v", derr)
			}
			resumed = rr
		}
	}
	if !resumed.ReissueTools {
		t.Fatalf("ReissueTools = false, want true")
	}
	if resumed.PendingCalls != 1 {
		t.Fatalf("PendingCalls = %d, want 1", resumed.PendingCalls)
	}
	// Count Scheduled events: one orphan (original, no matching Completed
	// because it was truncated) + one fresh from reissue = 2.
	var sched, comp int
	for _, ev := range merged {
		switch ev.Kind {
		case event.KindToolCallScheduled:
			sched++
		case event.KindToolCallCompleted:
			comp++
		}
	}
	if sched != 2 {
		t.Fatalf("ToolCallScheduled count = %d, want 2 (orphan + reissue)", sched)
	}
	if comp != 1 {
		t.Fatalf("ToolCallCompleted count = %d, want 1 (reissue only)", comp)
	}

	// Reissued schedule must use a fresh CallID, and the matching
	// Completed must reference it.
	var orphanID, freshID, completedID string
	for _, ev := range merged {
		switch ev.Kind {
		case event.KindToolCallScheduled:
			s, _ := ev.AsToolCallScheduled()
			if orphanID == "" {
				orphanID = s.CallID
			} else if freshID == "" {
				freshID = s.CallID
			}
		case event.KindToolCallCompleted:
			c, _ := ev.AsToolCallCompleted()
			completedID = c.CallID
		}
	}
	if orphanID == "" || freshID == "" {
		t.Fatalf("expected orphan + fresh ToolCallScheduled, got orphan=%q fresh=%q", orphanID, freshID)
	}
	if orphanID == freshID {
		t.Fatalf("reissue used orphan CallID %q; want a fresh one", orphanID)
	}
	if completedID != freshID {
		t.Fatalf("ToolCallCompleted.CallID = %q, want fresh %q", completedID, freshID)
	}
}

func TestResume_PendingToolCallRefuse(t *testing.T) {
	scripts := [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 10, OutputTokens: 5}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
	}
	cfg := starling.Config{Model: "gpt-4o-mini", MaxTurns: 4}
	log, runID := recordThenCrash(t, cfg, []tool.Tool{echoTool()}, "do it", scripts, 4)

	a := &starling.Agent{
		Provider: &cannedProvider{},
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   cfg,
	}
	_, err := a.ResumeWith(context.Background(), runID, "", starling.WithReissueTools(false))
	if !errors.Is(err, starling.ErrPartialToolCall) {
		t.Fatalf("err = %v, want ErrPartialToolCall", err)
	}
	// Must not have mutated the log.
	merged, _ := log.Read(context.Background(), runID)
	for _, ev := range merged {
		if ev.Kind == event.KindRunResumed {
			t.Fatalf("ResumeWith(false) should not have emitted RunResumed; found one at seq=%d", ev.Seq)
		}
	}
}

// capturingProvider wraps a script and records the Request passed to
// Stream. Used to inspect what conversation history a resumed run
// hands to the next provider call.
type capturingProvider struct {
	scripts [][]provider.StreamChunk
	i       int
	got     []*provider.Request
}

func (p *capturingProvider) Info() provider.Info {
	return provider.Info{ID: "capturing", APIVersion: "v0"}
}

func (p *capturingProvider) Stream(_ context.Context, req *provider.Request) (provider.EventStream, error) {
	p.got = append(p.got, req)
	if p.i >= len(p.scripts) {
		return nil, io.EOF
	}
	s := &cannedStream{chunks: p.scripts[p.i]}
	p.i++
	return s, nil
}

// TestResume_AfterToolCompleted_DecodesResultAsJSON verifies that the
// tool message rebuilt from a recorded ToolCallCompleted event carries
// JSON in Content (matching the live agent path), not the raw CBOR
// bytes the event stores. See resume.go's KindToolCallCompleted arm.
func TestResume_AfterToolCompleted_DecodesResultAsJSON(t *testing.T) {
	scripts := [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 10, OutputTokens: 5}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
	}
	cfg := starling.Config{Model: "gpt-4o-mini", MaxTurns: 4}

	// Run all 6 events of turn 1: RunStarted, TurnStarted, Assistant,
	// Scheduled, Completed, then crash before the next TurnStarted.
	log, runID := recordThenCrash(t, cfg, []tool.Tool{echoTool()}, "do it", scripts, 6)

	cap := &capturingProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "ok"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	a := &starling.Agent{
		Provider: cap,
		Tools:    []tool.Tool{echoTool()},
		Log:      log,
		Config:   cfg,
	}
	if _, err := a.Resume(context.Background(), runID, ""); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(cap.got) == 0 {
		t.Fatalf("no captured Stream call on resume")
	}

	// Find the tool-result message handed to the resumed provider.
	var toolMsg *provider.Message
	for i := range cap.got[0].Messages {
		m := &cap.got[0].Messages[i]
		if m.Role == provider.RoleTool && m.ToolResult != nil && m.ToolResult.CallID == "c1" {
			toolMsg = m
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("captured request has no tool result for c1: %+v", cap.got[0].Messages)
	}

	// Content must parse as JSON and equal the original tool output.
	var got map[string]any
	if err := json.Unmarshal([]byte(toolMsg.ToolResult.Content), &got); err != nil {
		t.Fatalf("tool result Content is not valid JSON: %q (err %v)", toolMsg.ToolResult.Content, err)
	}
	want := map[string]any{"got": "ping"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool result mismatch: got %v, want %v", got, want)
	}
}

func TestResume_NamespaceMismatch(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{
		Provider:  &cannedProvider{},
		Log:       log,
		Config:    starling.Config{Model: "gpt-4o-mini"},
		Namespace: "foo",
	}
	_, err := a.Resume(context.Background(), "bar/xyz", "")
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("err = %v, want namespace-mismatch error", err)
	}
}

func TestResume_SchemaMismatch(t *testing.T) {
	// Hand-craft a RunStarted with bogus SchemaVersion.
	log := eventlog.NewInMemory()
	defer log.Close()
	payload, err := event.EncodePayload(event.RunStarted{
		SchemaVersion: 999,
		Goal:          "g",
		ProviderID:    "canned",
	})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	ev := event.Event{
		RunID:     "rid",
		Seq:       1,
		PrevHash:  nil,
		Timestamp: 1,
		Kind:      event.KindRunStarted,
		Payload:   payload,
	}
	if err := log.Append(context.Background(), "rid", ev); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := &starling.Agent{
		Provider: &cannedProvider{},
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini"},
	}
	_, err = a.Resume(context.Background(), "rid", "")
	if !errors.Is(err, starling.ErrSchemaVersionMismatch) {
		t.Fatalf("err = %v, want ErrSchemaVersionMismatch", err)
	}
}
