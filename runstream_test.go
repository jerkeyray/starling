package starling_test

import (
	"context"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/starlingtest"
	"github.com/jerkeyray/starling/tool"
)

func TestRunStream_TextOnly_DoneIsLast(t *testing.T) {
	p := &starlingtest.ScriptedProvider{Scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "hi"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{Provider: p, Log: log, Config: starling.Config{Model: "gpt-4o-mini", MaxTurns: 2}}

	_, ch, err := a.RunStream(context.Background(), "say hi")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var got []starling.AgentEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) < 2 {
		t.Fatalf("got %d events, want at least 2", len(got))
	}
	if _, ok := got[0].(starling.TextDelta); !ok {
		t.Fatalf("first event = %T, want TextDelta", got[0])
	}
	last, ok := got[len(got)-1].(starling.Done)
	if !ok {
		t.Fatalf("last event = %T, want Done", got[len(got)-1])
	}
	if last.TerminalKind != event.KindRunCompleted {
		t.Fatalf("Done.TerminalKind = %s, want RunCompleted", last.TerminalKind)
	}
	if last.FinalText != "hi" {
		t.Fatalf("Done.FinalText = %q", last.FinalText)
	}
}

func TestRunStream_ToolRoundTrip_EmitsStartAndEnd(t *testing.T) {
	p := &starlingtest.ScriptedProvider{Scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 5, OutputTokens: 3}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
		{
			{Kind: provider.ChunkText, Text: "got ping"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 8, OutputTokens: 2}},
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

	_, ch, err := a.RunStream(context.Background(), "do it")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var sawStart, sawEnd, sawDone bool
	for ev := range ch {
		switch v := ev.(type) {
		case starling.ToolCallStarted:
			sawStart = true
			if v.Tool != "echo" || v.CallID != "c1" {
				t.Fatalf("Start = %+v", v)
			}
		case starling.ToolCallEnded:
			sawEnd = true
			if v.Tool != "echo" {
				t.Fatalf("End.Tool = %q, want echo (CallID lookup)", v.Tool)
			}
			if v.Err != nil {
				t.Fatalf("End.Err = %v, want nil", v.Err)
			}
		case starling.Done:
			sawDone = true
		}
	}
	if !sawStart || !sawEnd || !sawDone {
		t.Fatalf("missing event: start=%v end=%v done=%v", sawStart, sawEnd, sawDone)
	}
}
