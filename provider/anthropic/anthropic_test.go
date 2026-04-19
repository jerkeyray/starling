package anthropic

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

// sseFrame is one SSE record: an event-name line plus one data JSON line.
type sseFrame struct {
	Event string
	Data  string
}

// sseServer spins up an httptest.Server that responds to any request
// with a fixed sequence of SSE frames. Every frame is written as
// `event: <name>\ndata: <payload>\n\n`.
func sseServer(t *testing.T, frames []sseFrame, headers map[string]string) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.Event, f.Data)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	return httptest.NewServer(h)
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

func messageStart(id string, inputTokens int64) sseFrame {
	return sseFrame{
		Event: "message_start",
		Data: fmt.Sprintf(
			`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
			id, inputTokens,
		),
	}
}

func contentBlockStart(index int, typ string, extra string) sseFrame {
	return sseFrame{
		Event: "content_block_start",
		Data: fmt.Sprintf(
			`{"type":"content_block_start","index":%d,"content_block":{"type":%q%s}}`,
			index, typ, extra,
		),
	}
}

func textDelta(index int, text string) sseFrame {
	return sseFrame{
		Event: "content_block_delta",
		Data: fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`,
			index, text,
		),
	}
}

func inputJSONDelta(index int, partial string) sseFrame {
	return sseFrame{
		Event: "content_block_delta",
		Data: fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`,
			index, partial,
		),
	}
}

func thinkingDelta(index int, thinking string) sseFrame {
	return sseFrame{
		Event: "content_block_delta",
		Data: fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%q}}`,
			index, thinking,
		),
	}
}

func signatureDelta(index int, sig string) sseFrame {
	return sseFrame{
		Event: "content_block_delta",
		Data: fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":%q}}`,
			index, sig,
		),
	}
}

func contentBlockStop(index int) sseFrame {
	return sseFrame{
		Event: "content_block_stop",
		Data:  fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index),
	}
}

func messageDelta(stopReason string, outputTokens int64) sseFrame {
	return sseFrame{
		Event: "message_delta",
		Data: fmt.Sprintf(
			`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
			stopReason, outputTokens,
		),
	}
}

func messageDeltaWithCache(stopReason string, outputTokens, cacheCreate, cacheRead int64) sseFrame {
	return sseFrame{
		Event: "message_delta",
		Data: fmt.Sprintf(
			`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}`,
			stopReason, outputTokens, cacheCreate, cacheRead,
		),
	}
}

func messageStop() sseFrame {
	return sseFrame{Event: "message_stop", Data: `{"type":"message_stop"}`}
}

// ---- tests -----------------------------------------------------------------

func TestStream_TextOnly(t *testing.T) {
	frames := []sseFrame{
		messageStart("msg_1", 10),
		contentBlockStart(0, "text", `,"text":""`),
		textDelta(0, "Hello"),
		textDelta(0, " world"),
		contentBlockStop(0),
		messageDelta("end_turn", 5),
		messageStop(),
	}
	srv := sseServer(t, frames, map[string]string{"request-id": "req_abc"})
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "claude-test", MaxOutputTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	chunks := drain(t, stream)

	var text strings.Builder
	var end *provider.StreamChunk
	var usage *provider.UsageUpdate
	for i := range chunks {
		c := chunks[i]
		switch c.Kind {
		case provider.ChunkText:
			text.WriteString(c.Text)
		case provider.ChunkUsage:
			usage = c.Usage
		case provider.ChunkEnd:
			end = &c
		}
	}
	if text.String() != "Hello world" {
		t.Fatalf("text = %q, want %q", text.String(), "Hello world")
	}
	if end == nil {
		t.Fatalf("no ChunkEnd")
	}
	if end.StopReason != "end_turn" {
		t.Fatalf("StopReason = %q, want end_turn", end.StopReason)
	}
	if end.ProviderReqID != "req_abc" && end.ProviderReqID != "msg_1" {
		// request-id header preferred, message id fallback — either is valid.
		t.Fatalf("ProviderReqID = %q, want req_abc or msg_1", end.ProviderReqID)
	}
	if len(end.RawResponseHash) != 32 {
		t.Fatalf("RawResponseHash len = %d, want 32", len(end.RawResponseHash))
	}
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v, want in=10 out=5", usage)
	}
}

func TestStream_ToolUseMultiDelta(t *testing.T) {
	frames := []sseFrame{
		messageStart("msg_2", 8),
		contentBlockStart(0, "tool_use", `,"id":"toolu_1","name":"get_weather","input":{}`),
		inputJSONDelta(0, `{"cit`),
		inputJSONDelta(0, `y":"SF"}`),
		contentBlockStop(0),
		messageDelta("tool_use", 12),
		messageStop(),
	}
	srv := sseServer(t, frames, nil)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "claude-test", MaxOutputTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	chunks := drain(t, stream)

	var callID, name string
	var args strings.Builder
	sawEnd := false
	for _, c := range chunks {
		switch c.Kind {
		case provider.ChunkToolUseStart:
			callID = c.ToolUse.CallID
			name = c.ToolUse.Name
		case provider.ChunkToolUseDelta:
			args.WriteString(c.ToolUse.ArgsDelta)
		case provider.ChunkToolUseEnd:
			sawEnd = true
		}
	}
	if callID != "toolu_1" || name != "get_weather" {
		t.Fatalf("got call=%q name=%q, want toolu_1 get_weather", callID, name)
	}
	if args.String() != `{"city":"SF"}` {
		t.Fatalf("args = %q, want {\"city\":\"SF\"}", args.String())
	}
	if !sawEnd {
		t.Fatalf("missing ChunkToolUseEnd")
	}
	// Round-trip args through JSON to confirm well-formed.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args.String()), &decoded); err != nil {
		t.Fatalf("args JSON: %v", err)
	}
}

func TestStream_ThinkingWithSignature(t *testing.T) {
	frames := []sseFrame{
		messageStart("msg_3", 20),
		contentBlockStart(0, "thinking", `,"thinking":"","signature":""`),
		thinkingDelta(0, "let me think"),
		thinkingDelta(0, " about this"),
		signatureDelta(0, "SIGBYTES"),
		contentBlockStop(0),
		contentBlockStart(1, "text", `,"text":""`),
		textDelta(1, "answer"),
		contentBlockStop(1),
		messageDelta("end_turn", 9),
		messageStop(),
	}
	srv := sseServer(t, frames, nil)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "claude-test", MaxOutputTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	chunks := drain(t, stream)

	// Expect: ChunkReasoning("let me think"), ChunkReasoning(" about this"),
	// ChunkReasoning{Signature:"SIGBYTES"}, ChunkText("answer"), ChunkUsage, ChunkEnd.
	var (
		reasoningText strings.Builder
		sig           []byte
		text          string
	)
	for _, c := range chunks {
		switch c.Kind {
		case provider.ChunkReasoning:
			reasoningText.WriteString(c.Text)
			if len(c.Signature) > 0 {
				sig = c.Signature
			}
		case provider.ChunkText:
			text += c.Text
		}
	}
	if reasoningText.String() != "let me think about this" {
		t.Fatalf("reasoning = %q", reasoningText.String())
	}
	if string(sig) != "SIGBYTES" {
		t.Fatalf("signature = %q, want SIGBYTES", sig)
	}
	if text != "answer" {
		t.Fatalf("text = %q", text)
	}
}

func TestStream_RedactedThinking(t *testing.T) {
	frames := []sseFrame{
		messageStart("msg_4", 5),
		contentBlockStart(0, "redacted_thinking", `,"data":"OPAQUEDATA"`),
		signatureDelta(0, "REDSIG"),
		contentBlockStop(0),
		messageDelta("end_turn", 1),
		messageStop(),
	}
	srv := sseServer(t, frames, nil)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "claude-test", MaxOutputTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	chunks := drain(t, stream)

	var found *provider.StreamChunk
	for i := range chunks {
		if chunks[i].Kind == provider.ChunkRedactedThinking {
			found = &chunks[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no ChunkRedactedThinking; chunks=%+v", chunks)
	}
	if found.Text != "OPAQUEDATA" {
		t.Fatalf("Text = %q, want OPAQUEDATA", found.Text)
	}
	if string(found.Signature) != "REDSIG" {
		t.Fatalf("Signature = %q, want REDSIG", found.Signature)
	}
}

func TestStream_CacheTokensPropagate(t *testing.T) {
	frames := []sseFrame{
		messageStart("msg_5", 100),
		contentBlockStart(0, "text", `,"text":""`),
		textDelta(0, "ok"),
		contentBlockStop(0),
		messageDeltaWithCache("end_turn", 3, 50, 40),
		messageStop(),
	}
	srv := sseServer(t, frames, nil)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "claude-test", MaxOutputTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	chunks := drain(t, stream)

	var u *provider.UsageUpdate
	for _, c := range chunks {
		if c.Kind == provider.ChunkUsage {
			u = c.Usage
		}
	}
	if u == nil {
		t.Fatalf("no ChunkUsage")
	}
	if u.CacheCreateTokens != 50 || u.CacheReadTokens != 40 {
		t.Fatalf("cache = create %d read %d, want 50/40", u.CacheCreateTokens, u.CacheReadTokens)
	}
}

func TestStream_ErrorEventPropagates(t *testing.T) {
	frames := []sseFrame{
		messageStart("msg_6", 1),
		{Event: "error", Data: `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`},
	}
	srv := sseServer(t, frames, nil)
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "claude-test", MaxOutputTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, nerr := stream.Next(ctx)
		if nerr == io.EOF {
			t.Fatalf("stream reached EOF without surfacing error event")
		}
		if nerr != nil {
			if !strings.Contains(nerr.Error(), "overloaded_error") {
				t.Fatalf("error = %v, want overloaded_error", nerr)
			}
			return
		}
	}
}

func TestStream_RawResponseHashStable(t *testing.T) {
	frames := []sseFrame{
		messageStart("msg_7", 2),
		contentBlockStart(0, "text", `,"text":""`),
		textDelta(0, "x"),
		contentBlockStop(0),
		messageDelta("end_turn", 1),
		messageStop(),
	}
	run := func() []byte {
		srv := sseServer(t, frames, nil)
		defer srv.Close()
		p := newTestProvider(t, srv.URL)
		s, err := p.Stream(context.Background(), &provider.Request{Model: "claude-test", MaxOutputTokens: 1024})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		defer s.Close()
		chunks := drain(t, s)
		for _, c := range chunks {
			if c.Kind == provider.ChunkEnd {
				return c.RawResponseHash
			}
		}
		t.Fatalf("no ChunkEnd")
		return nil
	}
	a := run()
	b := run()
	if len(a) != 32 || len(b) != 32 {
		t.Fatalf("hash lens = %d / %d, want 32", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("hash differs: %x vs %x", a, b)
		}
	}
}
