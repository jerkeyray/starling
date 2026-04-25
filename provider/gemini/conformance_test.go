package gemini

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/conformance"
)

type confAdapter struct{}

func (confAdapter) Name() string { return "gemini" }

func (confAdapter) Capabilities() conformance.Capabilities {
	p, _ := New(WithAPIKey("test-key"))
	return p.(provider.Capabler).Capabilities()
}

func (confAdapter) NewProvider(t *testing.T, s conformance.Scenario) provider.Provider {
	t.Helper()
	switch s {
	case conformance.ScenarioTextOnly:
		srv := sseServer(t, []sseFrame{
			textFrame("hi"),
			finishFrame("STOP", 1, 1),
		})
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	case conformance.ScenarioToolCall:
		srv := sseServer(t, []sseFrame{
			toolCallFrame("call-1", "search", `{"q":"go"}`),
			finishFrame("STOP", 1, 1),
		})
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	case conformance.ScenarioStreamError:
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return newTestProvider(t, srv.URL)
	}
	t.Fatalf("gemini conformance: unhandled scenario %d", s)
	return nil
}

func TestConformance(t *testing.T) {
	conformance.Run(t, confAdapter{})
}
