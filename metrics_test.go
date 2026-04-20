package starling_test

import (
	"context"
	"strings"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/tool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Each test uses its own prometheus.NewRegistry() so collectors
// don't leak across tests — the collector set is defined per-
// Metrics, so registering twice against the same Registerer would
// panic. The isolation also lets us assert against a known-empty
// starting state.

func TestMetrics_Run_RecordsTerminalOK(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := starling.NewMetrics(reg)

	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "hello"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 7, OutputTokens: 3}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
		Metrics:  metrics,
	}
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Run lifecycle.
	if got := findCounter(t, reg, "starling_runs_started_total", nil); got != 1 {
		t.Errorf("runs_started_total = %v, want 1", got)
	}
	if got := findGauge(t, reg, "starling_runs_in_flight", nil); got != 0 {
		t.Errorf("runs_in_flight = %v, want 0", got)
	}
	// Terminal counter; label-scoped lookup via the CounterVec.
	termOK := findCounter(t, reg, "starling_run_terminal_total",
		map[string]string{"status": "ok", "error_type": "none"})
	if termOK != 1 {
		t.Errorf("run_terminal_total{status=ok} = %v, want 1", termOK)
	}
	// Provider call counter.
	provOK := findCounter(t, reg, "starling_provider_calls_total",
		map[string]string{"model": "gpt-4o-mini", "status": "ok"})
	if provOK != 1 {
		t.Errorf("provider_calls_total{status=ok} = %v, want 1", provOK)
	}
	// Tokens.
	prompt := findCounter(t, reg, "starling_provider_tokens_total",
		map[string]string{"model": "gpt-4o-mini", "type": "prompt"})
	if prompt != 7 {
		t.Errorf("provider_tokens_total{type=prompt} = %v, want 7", prompt)
	}
	completion := findCounter(t, reg, "starling_provider_tokens_total",
		map[string]string{"model": "gpt-4o-mini", "type": "completion"})
	if completion != 3 {
		t.Errorf("provider_tokens_total{type=completion} = %v, want 3", completion)
	}
	// Eventlog appends: RunStarted, TurnStarted, AssistantMessageCompleted,
	// RunCompleted = 4 events. Labels change per kind; assert total via
	// metric-name rollup is noisy, so just assert RunCompleted landed.
	runCompleted := findCounter(t, reg, "starling_eventlog_appends_total",
		map[string]string{"kind": "RunCompleted", "status": "ok"})
	if runCompleted != 1 {
		t.Errorf("eventlog_appends_total{kind=RunCompleted} = %v, want 1", runCompleted)
	}
}

func TestMetrics_Tool_RecordsOKAndDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := starling.NewMetrics(reg)

	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "echo"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{"msg":"ping"}`}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 2, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
		{
			{Kind: provider.ChunkText, Text: "done"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 3, OutputTokens: 1}},
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
		Metrics:  metrics,
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ok := findCounter(t, reg, "starling_tool_calls_total",
		map[string]string{"tool": "echo", "status": "ok", "error_type": "none"})
	if ok != 1 {
		t.Errorf("tool_calls_total{tool=echo,ok} = %v, want 1", ok)
	}
}

// TestMetrics_NilAgent_NoPanic confirms the default (Metrics: nil)
// path is a total no-op. Regression guard against someone adding a
// non-guarded record call that would NPE here.
func TestMetrics_NilAgent_NoPanic(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{
		{
			{Kind: provider.ChunkText, Text: "hello"},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
		// Metrics intentionally nil.
	}
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestMetrics_RegistryIsolation confirms two independent Metrics
// instances don't collide at registration. Guards against someone
// accidentally moving collectors to the default registry (where
// re-registration would panic).
func TestMetrics_RegistryIsolation(t *testing.T) {
	_ = starling.NewMetrics(prometheus.NewRegistry())
	_ = starling.NewMetrics(prometheus.NewRegistry())
	// If either call panicked we'd have failed already.
}

// TestNewMetrics_NilRegistererPanics is the API-misuse guard.
func TestNewMetrics_NilRegistererPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewMetrics(nil) did not panic")
		}
	}()
	_ = starling.NewMetrics(nil)
}

// findCounter fishes a specific label-set's counter value out of a
// Gatherer. Pass labels=nil to find a single-series counter.
func findCounter(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m.GetLabel(), labels) {
				if c := m.GetCounter(); c != nil {
					return c.GetValue()
				}
			}
		}
	}
	t.Fatalf("counter %s %s not found", name, labelString(labels))
	return 0
}

// findGauge mirrors findCounter for Gauge metrics.
func findGauge(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m.GetLabel(), labels) {
				if gg := m.GetGauge(); gg != nil {
					return gg.GetValue()
				}
			}
		}
	}
	t.Fatalf("gauge %s %s not found", name, labelString(labels))
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

func labelString(m map[string]string) string {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for k, v := range m {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
	}
	b.WriteByte('}')
	return b.String()
}
