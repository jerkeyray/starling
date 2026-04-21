package openrouter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jerkeyray/starling/provider"
)

// captureServer returns an httptest server that records the path and
// headers of the first incoming request, then writes a minimal SSE
// stream ending with [DONE] so the OpenAI adapter's stream parser
// terminates cleanly.
func captureServer(t *testing.T) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.mu.Lock()
		captured.path = r.URL.Path
		captured.authorization = r.Header.Get("Authorization")
		captured.referer = r.Header.Get("HTTP-Referer")
		captured.title = r.Header.Get("X-Title")
		captured.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Emit one finish frame + usage + [DONE] so the OpenAI stream
		// parser sees a clean end.
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":0,"model":"test","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})
	return httptest.NewServer(h), captured
}

type capturedRequest struct {
	mu            sync.Mutex
	path          string
	authorization string
	referer       string
	title         string
}

func (c *capturedRequest) snapshot() capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return capturedRequest{path: c.path, authorization: c.authorization, referer: c.referer, title: c.title}
}

// drain a stream to EOF so the request actually fires end-to-end.
func drain(t *testing.T, s provider.EventStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, err := s.Next(ctx)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
}

func runOneTurn(t *testing.T, p provider.Provider) {
	t.Helper()
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:    "openrouter/auto",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	drain(t, stream)
}

func TestNew_SetsOpenRouterBaseURL(t *testing.T) {
	srv, captured := captureServer(t)
	defer srv.Close()

	p, err := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runOneTurn(t, p)

	got := captured.snapshot()
	if got.path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", got.path)
	}
	if got.authorization != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", got.authorization)
	}
}

func TestNew_InjectsAttributionHeaders(t *testing.T) {
	srv, captured := captureServer(t)
	defer srv.Close()

	p, err := New(
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithHTTPReferer("https://example.test"),
		WithXTitle("starling-test"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runOneTurn(t, p)

	got := captured.snapshot()
	if got.referer != "https://example.test" {
		t.Errorf("HTTP-Referer = %q, want https://example.test", got.referer)
	}
	if got.title != "starling-test" {
		t.Errorf("X-Title = %q, want starling-test", got.title)
	}
}

func TestNew_NoHeadersWhenUnset(t *testing.T) {
	srv, captured := captureServer(t)
	defer srv.Close()

	p, err := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runOneTurn(t, p)

	got := captured.snapshot()
	if got.referer != "" {
		t.Errorf("HTTP-Referer = %q, want empty", got.referer)
	}
	if got.title != "" {
		t.Errorf("X-Title = %q, want empty", got.title)
	}
}

func TestNew_ProviderIDDefaultsAndOverrides(t *testing.T) {
	p, err := New(WithAPIKey("k"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.Info().ID; got != "openrouter" {
		t.Errorf("Info().ID = %q, want openrouter", got)
	}

	p2, err := New(WithAPIKey("k"), WithProviderID("openrouter-prod"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p2.Info().ID; got != "openrouter-prod" {
		t.Errorf("Info().ID = %q, want openrouter-prod", got)
	}
}

// TestNew_PreservesCustomTransport confirms that a caller-supplied
// http.Client's custom Transport is still invoked when attribution
// headers are also set (layering, not replacement).
func TestNew_PreservesCustomTransport(t *testing.T) {
	srv, captured := captureServer(t)
	defer srv.Close()

	var customCalls int
	customTransport := &countingTransport{inner: http.DefaultTransport, count: &customCalls}
	client := &http.Client{Transport: customTransport}

	p, err := New(
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithHTTPClient(client),
		WithHTTPReferer("https://example.test"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runOneTurn(t, p)

	if customCalls == 0 {
		t.Errorf("custom transport was not invoked")
	}
	if got := captured.snapshot().referer; got != "https://example.test" {
		t.Errorf("HTTP-Referer = %q, want https://example.test", got)
	}
}

type countingTransport struct {
	inner http.RoundTripper
	count *int
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*c.count++
	return c.inner.RoundTrip(req)
}
