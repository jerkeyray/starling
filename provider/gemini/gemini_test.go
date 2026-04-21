package gemini

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

	"github.com/jerkeyray/starling/provider"
)

// sseFrame is one SSE record: a single `data: <json>` line followed
// by a blank line. Gemini doesn't use the `event:` line.
type sseFrame struct {
	Data string
}

// sseServer spins up an httptest.Server that responds to any request
// with a fixed sequence of SSE frames. Each frame is written as
// `data: <payload>\n\n`.
func sseServer(t *testing.T, frames []sseFrame) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f.Data)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	return httptest.NewServer(h)
}

// capturingServer returns an httptest.Server plus a pointer that gets
// populated with the last request body seen. Useful for asserting the
// outgoing payload shape (system prompt, generationConfig, toolConfig).
func capturingServer(t *testing.T, frames []sseFrame) (*httptest.Server, *[]byte) {
	t.Helper()
	captured := new([]byte)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*captured = body
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f.Data)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	return httptest.NewServer(h), captured
}

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

// ---- canned frames ---------------------------------------------------------

// textFrame builds a streamGenerateContent chunk carrying a text delta.
func textFrame(text string) sseFrame {
	body := fmt.Sprintf(
		`{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]},"index":0}]}`,
		text,
	)
	return sseFrame{Data: body}
}

// finishFrame builds the terminal chunk with a finishReason and
// usageMetadata.
func finishFrame(reason string, prompt, candidates int) sseFrame {
	body := fmt.Sprintf(
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":%q,"index":0}],"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d},"responseId":"resp-xyz"}`,
		reason, prompt, candidates, prompt+candidates,
	)
	return sseFrame{Data: body}
}

// toolCallFrame builds a chunk carrying a functionCall part.
func toolCallFrame(id, name, argsJSON string) sseFrame {
	body := fmt.Sprintf(
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":%q,"name":%q,"args":%s}}]},"index":0}]}`,
		id, name, argsJSON,
	)
	return sseFrame{Data: body}
}

// ---- tests -----------------------------------------------------------------

func TestStream_TextCompletion(t *testing.T) {
	frames := []sseFrame{
		textFrame("hello "),
		textFrame("world"),
		finishFrame("STOP", 7, 3),
	}
	srv := sseServer(t, frames)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:    "gemini-2.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	got := drain(t, stream)

	// Expect: ChunkText, ChunkText, ChunkUsage, ChunkEnd.
	if len(got) != 4 {
		t.Fatalf("chunk count = %d, want 4. got=%v", len(got), got)
	}
	if got[0].Kind != provider.ChunkText || got[0].Text != "hello " {
		t.Errorf("chunk[0] = %+v, want ChunkText 'hello '", got[0])
	}
	if got[1].Kind != provider.ChunkText || got[1].Text != "world" {
		t.Errorf("chunk[1] = %+v, want ChunkText 'world'", got[1])
	}
	if got[2].Kind != provider.ChunkUsage || got[2].Usage == nil {
		t.Fatalf("chunk[2] = %+v, want ChunkUsage", got[2])
	}
	if got[2].Usage.InputTokens != 7 || got[2].Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want {7,3}", got[2].Usage)
	}
	end := got[3]
	if end.Kind != provider.ChunkEnd {
		t.Fatalf("chunk[3] = %+v, want ChunkEnd", end)
	}
	if end.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", end.StopReason, "stop")
	}
	if len(end.RawResponseHash) != 32 {
		t.Errorf("RawResponseHash len = %d, want 32", len(end.RawResponseHash))
	}
	if end.ProviderReqID != "resp-xyz" {
		t.Errorf("ProviderReqID = %q, want %q", end.ProviderReqID, "resp-xyz")
	}
}

func TestStream_ToolCall(t *testing.T) {
	frames := []sseFrame{
		toolCallFrame("call-1", "get_weather", `{"city":"Paris"}`),
		finishFrame("STOP", 12, 8),
	}
	srv := sseServer(t, frames)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:    "gemini-2.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "weather?"}},
		Tools: []provider.ToolDefinition{{
			Name:        "get_weather",
			Description: "Get the weather",
			Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	got := drain(t, stream)

	// Expect Start + Delta + End + Usage + End-of-stream.
	if len(got) != 5 {
		t.Fatalf("chunk count = %d, want 5. got=%+v", len(got), got)
	}
	if got[0].Kind != provider.ChunkToolUseStart {
		t.Errorf("chunk[0].Kind = %v, want ChunkToolUseStart", got[0].Kind)
	}
	if got[0].ToolUse == nil || got[0].ToolUse.CallID != "call-1" || got[0].ToolUse.Name != "get_weather" {
		t.Errorf("chunk[0].ToolUse = %+v", got[0].ToolUse)
	}
	if got[1].Kind != provider.ChunkToolUseDelta {
		t.Errorf("chunk[1].Kind = %v, want ChunkToolUseDelta", got[1].Kind)
	}
	if got[1].ToolUse == nil || !strings.Contains(got[1].ToolUse.ArgsDelta, `"Paris"`) {
		t.Errorf("chunk[1].ArgsDelta = %q, want args containing Paris", got[1].ToolUse.ArgsDelta)
	}
	if got[2].Kind != provider.ChunkToolUseEnd {
		t.Errorf("chunk[2].Kind = %v, want ChunkToolUseEnd", got[2].Kind)
	}
	// Usage + End tail.
	if got[3].Kind != provider.ChunkUsage {
		t.Errorf("chunk[3].Kind = %v, want ChunkUsage", got[3].Kind)
	}
	if got[4].Kind != provider.ChunkEnd {
		t.Errorf("chunk[4].Kind = %v, want ChunkEnd", got[4].Kind)
	}
	// Tool-call finishes with STOP per Gemini, but we normalize to
	// "tool_use" so the agent loop branches correctly.
	if got[4].StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want %q", got[4].StopReason, "tool_use")
	}
}

func TestStream_SystemPromptLandsInRequestBody(t *testing.T) {
	srv, captured := capturingServer(t, []sseFrame{finishFrame("STOP", 1, 1)})
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:        "gemini-2.5-flash",
		SystemPrompt: "You are a terse assistant.",
		Messages:     []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, stream)
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("unmarshal body: %v; raw=%s", err, string(*captured))
	}
	sys, ok := body["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction not found. body=%s", string(*captured))
	}
	parts, _ := sys["parts"].([]any)
	if len(parts) == 0 {
		t.Fatalf("systemInstruction.parts empty. body=%s", string(*captured))
	}
	p0, _ := parts[0].(map[string]any)
	if txt, _ := p0["text"].(string); txt != "You are a terse assistant." {
		t.Errorf("systemInstruction.parts[0].text = %q", txt)
	}
	// The user message should NOT contain the system prompt text —
	// verifies we didn't synthesize a user turn by mistake.
	if strings.Contains(string(*captured), "terse assistant") {
		// It will contain it inside systemInstruction; check contents
		// separately.
		contents, _ := body["contents"].([]any)
		for _, c := range contents {
			cm, _ := c.(map[string]any)
			parts, _ := cm["parts"].([]any)
			for _, part := range parts {
				pm, _ := part.(map[string]any)
				if strings.Contains(fmt.Sprint(pm["text"]), "terse") {
					t.Errorf("system text leaked into contents turn: %v", pm)
				}
			}
		}
	}
}

func TestStream_FinishReasonMapping(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		{"stop", "STOP", "stop"},
		{"max_tokens", "MAX_TOKENS", "max_tokens"},
		{"safety", "SAFETY", "filtered"},
		{"recitation", "RECITATION", "filtered"},
		{"malformed", "MALFORMED_FUNCTION_CALL", "error"},
		{"other", "OTHER", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := sseServer(t, []sseFrame{finishFrame(tc.reason, 1, 1)})
			defer srv.Close()

			p := newTestProvider(t, srv.URL)
			stream, err := p.Stream(context.Background(), &provider.Request{
				Model:    "gemini-2.5-flash",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}},
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer stream.Close()

			chunks := drain(t, stream)
			end := chunks[len(chunks)-1]
			if end.Kind != provider.ChunkEnd {
				t.Fatalf("last chunk = %v, want ChunkEnd", end.Kind)
			}
			if end.StopReason != tc.want {
				t.Errorf("StopReason = %q, want %q", end.StopReason, tc.want)
			}
		})
	}
}

func TestBuildParams_MaxTokensAndStopSequences(t *testing.T) {
	srv, captured := capturingServer(t, []sseFrame{finishFrame("STOP", 1, 1)})
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:           "gemini-2.5-flash",
		MaxOutputTokens: 123,
		StopSequences:   []string{"END", "STOP"},
		Messages:        []provider.Message{{Role: provider.RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, stream)
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	gc, ok := body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing. body=%s", string(*captured))
	}
	if mot := fmt.Sprint(gc["maxOutputTokens"]); mot != "123" {
		t.Errorf("maxOutputTokens = %v, want 123", gc["maxOutputTokens"])
	}
	seqs, _ := gc["stopSequences"].([]any)
	if len(seqs) != 2 || seqs[0] != "END" || seqs[1] != "STOP" {
		t.Errorf("stopSequences = %v, want [END STOP]", seqs)
	}
}

func TestBuildParams_ToolChoiceNone(t *testing.T) {
	srv, captured := capturingServer(t, []sseFrame{finishFrame("STOP", 1, 1)})
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:      "gemini-2.5-flash",
		ToolChoice: "none",
		Messages:   []provider.Message{{Role: provider.RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, stream)
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*captured, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	tc, ok := body["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("toolConfig missing. body=%s", string(*captured))
	}
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if mode, _ := fcc["mode"].(string); mode != "NONE" {
		t.Errorf("mode = %q, want NONE", mode)
	}
}

func TestConvertMessages_ToolResultResolvesName(t *testing.T) {
	// An assistant turn emits a tool call, then a RoleTool message
	// carries the result. The outgoing Gemini body should have a
	// functionResponse whose name matches the earlier functionCall.
	srv, captured := capturingServer(t, []sseFrame{finishFrame("STOP", 1, 1)})
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model: "gemini-2.5-flash",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "get weather"},
			{
				Role:    provider.RoleAssistant,
				Content: "",
				ToolUses: []provider.ToolUse{{
					CallID: "call-9",
					Name:   "get_weather",
					Args:   json.RawMessage(`{"city":"Tokyo"}`),
				}},
			},
			{
				Role: provider.RoleTool,
				ToolResult: &provider.ToolResult{
					CallID:  "call-9",
					Content: `{"temp":21}`,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, stream)
	stream.Close()

	// Find the functionResponse inside the captured body and confirm
	// its name field matches the earlier functionCall.
	if !strings.Contains(string(*captured), `"name":"get_weather"`) {
		t.Errorf("captured body missing get_weather name. body=%s", string(*captured))
	}
	if !strings.Contains(string(*captured), `"id":"call-9"`) {
		t.Errorf("captured body missing call-9 id. body=%s", string(*captured))
	}
}

func TestInfo_DefaultsAndOverrides(t *testing.T) {
	p, err := New(WithAPIKey("x"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if info := p.Info(); info.ID != "gemini" || info.APIVersion != "v1beta" {
		t.Errorf("default Info = %+v, want {gemini v1beta}", info)
	}

	p2, err := New(WithAPIKey("x"), WithProviderID("gemini-proxy"), WithAPIVersion("v1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if info := p2.Info(); info.ID != "gemini-proxy" || info.APIVersion != "v1" {
		t.Errorf("overridden Info = %+v", info)
	}
}
