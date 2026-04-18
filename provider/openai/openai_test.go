package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	oai "github.com/openai/openai-go/v3"

	"github.com/jerkeyray/starling/provider"
)

// sseEvent is one record in a canned SSE response. Data is the JSON payload
// sent after "data: ".
type sseEvent struct {
	Data string
}

// sseServer spins up an httptest.Server that responds to any POST with a
// fixed sequence of SSE data frames followed by `data: [DONE]`.
func sseServer(t *testing.T, events []sseEvent, headers map[string]string) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev.Data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})
	return httptest.NewServer(h)
}

// textChunk builds a ChatCompletionChunk JSON with a text delta.
func textChunk(id string, content string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"content":%q},"finish_reason":""}]}`, id, content)
}

// toolStartChunk emits a ChunkToolUseStart-shaped delta (id + name).
func toolStartChunk(id string, index int, callID, name string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":""}]}`, id, index, callID, name)
}

// toolArgChunk emits a ChunkToolUseDelta-shaped delta (args fragment).
func toolArgChunk(id string, index int, args string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]},"finish_reason":""}]}`, id, index, args)
}

// finishChunk emits an empty delta with finish_reason set.
func finishChunk(id, reason string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, id, reason)
}

// usageChunk emits the terminal usage-only chunk (empty Choices).
func usageChunk(id string, prompt, completion, cached int) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":0,"model":"test","choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`, id, prompt, completion, prompt+completion, cached)
}

// newTestProvider points New() at the given test server URL.
func newTestProvider(t *testing.T, url string, extra ...Option) provider.Provider {
	t.Helper()
	opts := append([]Option{WithAPIKey("test-key"), WithBaseURL(url)}, extra...)
	p, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func drain(t *testing.T, s provider.EventStream) []provider.StreamChunk {
	t.Helper()
	var out []provider.StreamChunk
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		c, err := s.Next(ctx)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, c)
	}
}

func TestStream_TextOnly(t *testing.T) {
	srv := sseServer(t, []sseEvent{
		{Data: textChunk("c1", "hello ")},
		{Data: textChunk("c1", "world")},
		{Data: finishChunk("c1", "stop")},
		{Data: usageChunk("c1", 10, 5, 0)},
	}, nil)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:    "test",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	chunks := drain(t, stream)
	wantKinds := []provider.ChunkKind{
		provider.ChunkText, provider.ChunkText,
		provider.ChunkUsage, provider.ChunkEnd,
	}
	if len(chunks) != len(wantKinds) {
		t.Fatalf("got %d chunks, want %d: %+v", len(chunks), len(wantKinds), chunks)
	}
	for i, want := range wantKinds {
		if chunks[i].Kind != want {
			t.Fatalf("chunk[%d].Kind = %s, want %s", i, chunks[i].Kind, want)
		}
	}
	if chunks[0].Text != "hello " || chunks[1].Text != "world" {
		t.Fatalf("text content mismatch: %q %q", chunks[0].Text, chunks[1].Text)
	}
	if chunks[2].Usage == nil || chunks[2].Usage.InputTokens != 10 || chunks[2].Usage.OutputTokens != 5 {
		t.Fatalf("usage mismatch: %+v", chunks[2].Usage)
	}
	if chunks[3].StopReason != "stop" {
		t.Fatalf("StopReason = %q, want stop", chunks[3].StopReason)
	}
	if len(chunks[3].RawResponseHash) != 32 {
		t.Fatalf("RawResponseHash len = %d, want 32", len(chunks[3].RawResponseHash))
	}
}

func TestStream_ToolCall(t *testing.T) {
	srv := sseServer(t, []sseEvent{
		{Data: toolStartChunk("c1", 0, "call_abc", "get_weather")},
		{Data: toolArgChunk("c1", 0, `{"ci`)},
		{Data: toolArgChunk("c1", 0, `ty":"SF`)},
		{Data: toolArgChunk("c1", 0, `"}`)},
		{Data: finishChunk("c1", "tool_calls")},
		{Data: usageChunk("c1", 20, 7, 0)},
	}, nil)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	chunks := drain(t, stream)
	wantKinds := []provider.ChunkKind{
		provider.ChunkToolUseStart,
		provider.ChunkToolUseDelta,
		provider.ChunkToolUseDelta,
		provider.ChunkToolUseDelta,
		provider.ChunkToolUseEnd,
		provider.ChunkUsage,
		provider.ChunkEnd,
	}
	if len(chunks) != len(wantKinds) {
		t.Fatalf("got %d chunks, want %d: %+v", len(chunks), len(wantKinds), chunks)
	}
	for i, want := range wantKinds {
		if chunks[i].Kind != want {
			t.Fatalf("chunk[%d].Kind = %s, want %s", i, chunks[i].Kind, want)
		}
	}
	if chunks[0].ToolUse.CallID != "call_abc" || chunks[0].ToolUse.Name != "get_weather" {
		t.Fatalf("start chunk: %+v", chunks[0].ToolUse)
	}
	var reassembled strings.Builder
	for _, c := range chunks[1:4] {
		if c.ToolUse.CallID != "call_abc" {
			t.Fatalf("delta CallID = %q", c.ToolUse.CallID)
		}
		reassembled.WriteString(c.ToolUse.ArgsDelta)
	}
	if reassembled.String() != `{"city":"SF"}` {
		t.Fatalf("args reassembly = %q", reassembled.String())
	}
	if chunks[4].ToolUse.CallID != "call_abc" {
		t.Fatalf("end CallID = %q", chunks[4].ToolUse.CallID)
	}
	if chunks[6].StopReason != "tool_calls" {
		t.Fatalf("StopReason = %q, want tool_calls", chunks[6].StopReason)
	}
}

func TestStream_RawResponseHashDeterministic(t *testing.T) {
	events := []sseEvent{
		{Data: textChunk("c1", "hi")},
		{Data: finishChunk("c1", "stop")},
		{Data: usageChunk("c1", 1, 1, 0)},
	}
	srv := sseServer(t, events, nil)
	defer srv.Close()

	run := func() []byte {
		p := newTestProvider(t, srv.URL)
		stream, err := p.Stream(context.Background(), &provider.Request{Model: "test"})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		defer stream.Close()
		cs := drain(t, stream)
		return cs[len(cs)-1].RawResponseHash
	}
	a, b := run(), run()
	if len(a) == 0 || string(a) != string(b) {
		t.Fatalf("hash non-deterministic:\na=%x\nb=%x", a, b)
	}
}

func TestStream_ProviderReqIDCaptured(t *testing.T) {
	srv := sseServer(t, []sseEvent{
		{Data: textChunk("c1", "hi")},
		{Data: finishChunk("c1", "stop")},
		{Data: usageChunk("c1", 1, 1, 0)},
	}, map[string]string{"x-request-id": "req-test-123"})
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	chunks := drain(t, stream)
	end := chunks[len(chunks)-1]
	if end.ProviderReqID != "req-test-123" {
		t.Fatalf("ProviderReqID = %q, want req-test-123", end.ProviderReqID)
	}
}

func TestStream_CloseIdempotent(t *testing.T) {
	srv := sseServer(t, []sseEvent{{Data: textChunk("c1", "hi")}}, nil)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestInfo_DefaultsAndOverrides(t *testing.T) {
	def, err := New(WithAPIKey("x"))
	if err != nil {
		t.Fatalf("New default: %v", err)
	}
	if got := def.Info(); got.ID != "openai" || got.APIVersion != "v1" {
		t.Fatalf("default Info = %+v, want {openai v1}", got)
	}

	over, err := New(WithAPIKey("x"), WithProviderID("groq"), WithAPIVersion("v1-groq"))
	if err != nil {
		t.Fatalf("New override: %v", err)
	}
	if got := over.Info(); got.ID != "groq" || got.APIVersion != "v1-groq" {
		t.Fatalf("override Info = %+v, want {groq v1-groq}", got)
	}
}

func TestConvertMessage_AssistantWithToolCalls(t *testing.T) {
	msg, err := convertMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: "thinking...",
		ToolUses: []provider.ToolUse{
			{CallID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"city":"SF"}`)},
		},
	})
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if msg.OfAssistant == nil {
		t.Fatalf("expected OfAssistant set, got %+v", msg)
	}
	if !msg.OfAssistant.Content.OfString.Valid() || msg.OfAssistant.Content.OfString.Value != "thinking..." {
		t.Fatalf("content = %+v", msg.OfAssistant.Content)
	}
	if len(msg.OfAssistant.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d", len(msg.OfAssistant.ToolCalls))
	}
	tc := msg.OfAssistant.ToolCalls[0].OfFunction
	if tc == nil || tc.ID != "call_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"SF"}` {
		t.Fatalf("tool call = %+v", tc)
	}
}

func TestConvertMessage_ToolWithError(t *testing.T) {
	msg, err := convertMessage(provider.Message{
		Role: provider.RoleTool,
		ToolResult: &provider.ToolResult{
			CallID:  "call_1",
			Content: "bad input",
			IsError: true,
		},
	})
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if msg.OfTool == nil {
		t.Fatalf("expected OfTool set")
	}
	if got := msg.OfTool.Content.OfString.Value; got != "error: bad input" {
		t.Fatalf("content = %q, want 'error: bad input'", got)
	}
	if msg.OfTool.ToolCallID != "call_1" {
		t.Fatalf("ToolCallID = %q", msg.OfTool.ToolCallID)
	}
}

// Unused import guard so test file doesn't drop oai if the underlying
// exported types shift. Keeps future refactors honest.
var _ oai.ChatCompletionChunk
