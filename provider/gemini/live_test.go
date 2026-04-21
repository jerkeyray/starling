package gemini

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/provider"
)

// TestLive_TextCompletion is a sanity check against the real Gemini
// API. Gated by GEMINI_API_KEY so regular `go test ./...` stays
// offline and hermetic. Run with:
//
//	GEMINI_API_KEY=... go test -race -run Live ./provider/gemini
func TestLive_TextCompletion(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live test")
	}

	p, err := New(WithAPIKey(key))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := p.Stream(ctx, &provider.Request{
		Model:           "gemini-2.5-flash",
		MaxOutputTokens: 64,
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: "Say the word 'pong' and nothing else.",
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var sawEnd bool
	var lastUsage provider.UsageUpdate
	for {
		c, err := stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch c.Kind {
		case provider.ChunkText:
			text.WriteString(c.Text)
		case provider.ChunkUsage:
			if c.Usage != nil {
				lastUsage = *c.Usage
			}
		case provider.ChunkEnd:
			sawEnd = true
			if c.StopReason == "" {
				t.Errorf("ChunkEnd has empty StopReason")
			}
			if len(c.RawResponseHash) != 32 {
				t.Errorf("RawResponseHash len = %d", len(c.RawResponseHash))
			}
		}
	}
	if !sawEnd {
		t.Fatalf("no ChunkEnd received")
	}
	if text.Len() == 0 {
		t.Fatalf("no text emitted")
	}
	if lastUsage.InputTokens == 0 || lastUsage.OutputTokens == 0 {
		t.Errorf("usage not populated: %+v", lastUsage)
	}
	t.Logf("response=%q usage=%+v", text.String(), lastUsage)
}

// TestLive_ToolCall asks the model to call a simple tool. Confirms
// the one-shot args delivery + tool_use stop reason contract.
func TestLive_ToolCall(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live test")
	}

	p, err := New(WithAPIKey(key))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"The city name"}},"required":["city"]}`)

	stream, err := p.Stream(ctx, &provider.Request{
		Model: "gemini-2.5-flash",
		Tools: []provider.ToolDefinition{{
			Name:        "get_weather",
			Description: "Look up the current weather for a city.",
			Schema:      schema,
		}},
		ToolChoice: "any",
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: "What's the weather in Tokyo? Use the tool.",
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var sawStart, sawDelta, sawEnd bool
	var stopReason string
	for {
		c, err := stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch c.Kind {
		case provider.ChunkToolUseStart:
			sawStart = true
		case provider.ChunkToolUseDelta:
			sawDelta = true
		case provider.ChunkEnd:
			sawEnd = true
			stopReason = c.StopReason
		}
	}
	if !sawStart || !sawDelta || !sawEnd {
		t.Fatalf("expected Start/Delta/End; got start=%v delta=%v end=%v", sawStart, sawDelta, sawEnd)
	}
	if stopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", stopReason)
	}
}
