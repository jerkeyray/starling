package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/conformance"
)

type confAdapter struct{}

func (confAdapter) Name() string { return "openai" }

func (confAdapter) Capabilities() conformance.Capabilities {
	p, _ := New(WithAPIKey("test-key"))
	return p.(provider.Capabler).Capabilities()
}

func (confAdapter) NewProvider(t *testing.T, s conformance.Scenario) provider.Provider {
	t.Helper()
	switch s {
	case conformance.ScenarioTextOnly:
		srv := sseServer(t, []sseEvent{
			{Data: textChunk("c1", "hi")},
			{Data: finishChunk("c1", "stop")},
			{Data: usageChunk("c1", 1, 1, 0)},
		}, map[string]string{"x-request-id": "req-conf-1"})
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	case conformance.ScenarioToolCall:
		srv := sseServer(t, []sseEvent{
			{Data: toolStartChunk("c1", 0, "call-1", "search")},
			{Data: toolArgChunk("c1", 0, `{"q":"go"}`)},
			{Data: finishChunk("c1", "tool_calls")},
			{Data: usageChunk("c1", 1, 1, 0)},
		}, nil)
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	case conformance.ScenarioStreamError:
		// Server hangs up after writing one frame — surfaces as a
		// non-EOF error from Next.
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusInternalServerError)
		})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	}
	t.Fatalf("openai conformance: unhandled scenario %d", s)
	return nil
}

func TestConformance(t *testing.T) {
	conformance.Run(t, confAdapter{})
}
