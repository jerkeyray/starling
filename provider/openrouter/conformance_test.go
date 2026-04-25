package openrouter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/conformance"
)

// scenarioServer mints an httptest.Server that replays a fixed SSE
// script for one request. The OpenRouter adapter delegates to OpenAI's
// stream parser, so the wire format here is OpenAI Chat Completions.
func scenarioServer(t *testing.T, frames []string, headers map[string]string) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
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

type confAdapter struct{}

func (confAdapter) Name() string { return "openrouter" }

func (confAdapter) Capabilities() conformance.Capabilities {
	p, _ := New(WithAPIKey("test-key"))
	return p.(provider.Capabler).Capabilities()
}

func (confAdapter) NewProvider(t *testing.T, s conformance.Scenario) provider.Provider {
	t.Helper()
	switch s {
	case conformance.ScenarioTextOnly:
		srv := scenarioServer(t, []string{
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":""}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		}, map[string]string{"x-request-id": "req-conf-1"})
		t.Cleanup(srv.Close)
		p, err := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
		if err != nil {
			t.Fatalf("openrouter.New: %v", err)
		}
		return p
	case conformance.ScenarioToolCall:
		srv := scenarioServer(t, []string{
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"search","arguments":""}}]},"finish_reason":""}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"go\"}"}}]},"finish_reason":""}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		}, nil)
		t.Cleanup(srv.Close)
		p, err := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
		if err != nil {
			t.Fatalf("openrouter.New: %v", err)
		}
		return p
	case conformance.ScenarioStreamError:
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		p, err := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
		if err != nil {
			t.Fatalf("openrouter.New: %v", err)
		}
		return p
	}
	t.Fatalf("openrouter conformance: unhandled scenario %d", s)
	return nil
}

func TestConformance(t *testing.T) {
	conformance.Run(t, confAdapter{})
}
