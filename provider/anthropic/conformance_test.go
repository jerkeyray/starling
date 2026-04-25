package anthropic

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/conformance"
)

type confAdapter struct{}

func (confAdapter) Name() string { return "anthropic" }

func (confAdapter) Capabilities() conformance.Capabilities {
	p, _ := New(WithAPIKey("test-key"))
	return p.(provider.Capabler).Capabilities()
}

func (confAdapter) NewProvider(t *testing.T, s conformance.Scenario) provider.Provider {
	t.Helper()
	switch s {
	case conformance.ScenarioTextOnly:
		frames := []sseFrame{
			messageStart("msg_conf_1", 5),
			contentBlockStart(0, "text", `,"text":""`),
			textDelta(0, "hi"),
			contentBlockStop(0),
			messageDelta("end_turn", 2),
			messageStop(),
		}
		srv := sseServer(t, frames, map[string]string{"x-request-id": "req-conf-1"})
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	case conformance.ScenarioToolCall:
		frames := []sseFrame{
			messageStart("msg_conf_2", 5),
			contentBlockStart(0, "tool_use", `,"id":"call-1","name":"search","input":{}`),
			inputJSONDelta(0, `{"q":"go"}`),
			contentBlockStop(0),
			messageDelta("tool_use", 5),
			messageStop(),
		}
		srv := sseServer(t, frames, nil)
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	case conformance.ScenarioStreamError:
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusInternalServerError)
		})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	}
	t.Fatalf("anthropic conformance: unhandled scenario %d", s)
	return nil
}

func TestConformance(t *testing.T) {
	conformance.Run(t, confAdapter{})
}
