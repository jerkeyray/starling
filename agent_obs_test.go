package starling_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestAgent_Logger_LifecycleAttrs verifies that Config.Logger receives
// "run started" + "run completed" records, and that every record
// carries the run_id attribute the rest of the trace stack expects.
func TestAgent_Logger_LifecycleAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(h)

	p := &cannedProvider{scripts: [][]provider.StreamChunk{{
		{Kind: provider.ChunkText, Text: "hi"},
		{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "m", MaxTurns: 2, Logger: logger},
	}
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Parse newline-delimited JSON records.
	type rec struct {
		Msg   string `json:"msg"`
		RunID string `json:"run_id"`
		Level string `json:"level"`
		Kind  string `json:"kind"`
	}
	var records []rec
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r rec
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, r)
	}
	if len(records) < 2 {
		t.Fatalf("got %d records, want >=2: %s", len(records), buf.String())
	}

	// Every record must carry run_id matching the run's RunID.
	for _, r := range records {
		if r.RunID != res.RunID {
			t.Fatalf("record %q: run_id = %q, want %q", r.Msg, r.RunID, res.RunID)
		}
	}

	// Specific lifecycle messages must be present.
	want := map[string]bool{"run started": false, "run completed": false}
	for _, r := range records {
		if _, ok := want[r.Msg]; ok {
			want[r.Msg] = true
		}
	}
	for msg, seen := range want {
		if !seen {
			t.Fatalf("missing log record %q; got: %s", msg, buf.String())
		}
	}

	// The "run completed" record must include kind=RunCompleted.
	for _, r := range records {
		if r.Msg == "run completed" && r.Kind != "RunCompleted" {
			t.Fatalf("run completed kind = %q, want RunCompleted", r.Kind)
		}
	}
}

// TestAgent_Logger_FailedRunIsErrorLevel pins the escalation: a failed
// run must emit a record at ERROR level so operators can alert on it.
func TestAgent_Logger_FailedRunIsErrorLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Provider whose Stream returns an error → react surfaces it →
	// emitTerminal classifies as RunFailed{provider}.
	p := &errProvider{}
	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "m", MaxTurns: 2, Logger: logger},
	}
	if _, err := a.Run(context.Background(), "go"); err == nil {
		t.Fatal("want error")
	}

	if !strings.Contains(buf.String(), `"level":"ERROR"`) ||
		!strings.Contains(buf.String(), `"msg":"run failed"`) {
		t.Fatalf("missing ERROR-level run failed record:\n%s", buf.String())
	}
}

// TestAgent_OTel_SpanTree verifies the span shape produced by a
// successful run: agent.run > agent.turn > agent.llm_call. Uses
// OTel's in-memory exporter so no external collector is required.
func TestAgent_OTel_SpanTree(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	p := &cannedProvider{scripts: [][]provider.StreamChunk{{
		{Kind: provider.ChunkText, Text: "hi"},
		{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}}}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "m", MaxTurns: 2},
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	spans := exporter.GetSpans()
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	sort.Strings(names)

	want := []string{"agent.llm_call", "agent.run", "agent.turn"}
	if !equalStringSlices(names, want) {
		t.Fatalf("span names = %v, want %v", names, want)
	}

	// agent.run must carry run_id; agent.llm_call must carry model.
	for _, s := range spans {
		switch s.Name {
		case "agent.run":
			if !hasAttr(s.Attributes, "run_id") {
				t.Fatalf("agent.run missing run_id attr")
			}
		case "agent.llm_call":
			if !hasAttr(s.Attributes, "model") {
				t.Fatalf("agent.llm_call missing model attr")
			}
		}
	}

	// Parent linkage: agent.turn parent should be agent.run; the LLM
	// span parent should be agent.turn.
	byName := indexByName(spans)
	runSpan := byName["agent.run"]
	turnSpan := byName["agent.turn"]
	llmSpan := byName["agent.llm_call"]
	if turnSpan.Parent.SpanID() != runSpan.SpanContext.SpanID() {
		t.Fatalf("agent.turn parent = %s, want agent.run", turnSpan.Parent.SpanID())
	}
	if llmSpan.Parent.SpanID() != turnSpan.SpanContext.SpanID() {
		t.Fatalf("agent.llm_call parent = %s, want agent.turn", llmSpan.Parent.SpanID())
	}
}

// TestAgent_ProviderStreamOpenError_ClassifiedAsProvider pins that an
// error returned from Provider.Stream surfaces as RunFailed{provider}
// and that the returned error wraps *starling.ProviderError so callers
// can route on it with errors.As.
func TestAgent_ProviderStreamOpenError_ClassifiedAsProvider(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{
		Provider: &openErrProvider{},
		Log:      log,
		Config:   starling.Config{Model: "m", MaxTurns: 2},
	}
	res, err := a.Run(context.Background(), "go")
	if err == nil {
		t.Fatal("want error from Run")
	}
	var pe *starling.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want errors.As to *starling.ProviderError", err, err)
	}
	if pe.Provider != "open-err" {
		t.Errorf("ProviderError.Provider = %q, want open-err", pe.Provider)
	}
	if res == nil || res.RunID == "" {
		t.Fatal("Run returned nil result despite terminal emission")
	}
	evs, _ := log.Read(context.Background(), res.RunID)
	last := evs[len(evs)-1]
	if last.Kind != event.KindRunFailed {
		t.Fatalf("last event = %s, want RunFailed", last.Kind)
	}
	rf, _ := last.AsRunFailed()
	if rf.ErrorType != "provider" {
		t.Fatalf("RunFailed.ErrorType = %q, want provider", rf.ErrorType)
	}
}

// TestAgent_ProviderStreamMidDrainError_ClassifiedAsProvider pins the
// same contract for an error returned mid-drain from EventStream.Next.
func TestAgent_ProviderStreamMidDrainError_ClassifiedAsProvider(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{
		Provider: &errProvider{},
		Log:      log,
		Config:   starling.Config{Model: "m", MaxTurns: 2},
	}
	res, err := a.Run(context.Background(), "go")
	if err == nil {
		t.Fatal("want error from Run")
	}
	var pe *starling.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want errors.As to *starling.ProviderError", err, err)
	}
	if pe.Provider != "err" {
		t.Errorf("ProviderError.Provider = %q, want err", pe.Provider)
	}
	evs, _ := log.Read(context.Background(), res.RunID)
	last := evs[len(evs)-1]
	if last.Kind != event.KindRunFailed {
		t.Fatalf("last event = %s, want RunFailed", last.Kind)
	}
	rf, _ := last.AsRunFailed()
	if rf.ErrorType != "provider" {
		t.Fatalf("RunFailed.ErrorType = %q, want provider", rf.ErrorType)
	}
}

// ---- helpers -------------------------------------------------------------

// openErrProvider returns an error from Stream itself (the open path).
type openErrProvider struct{}

func (openErrProvider) Info() provider.Info {
	return provider.Info{ID: "open-err", APIVersion: "v0"}
}
func (openErrProvider) Stream(_ context.Context, _ *provider.Request) (provider.EventStream, error) {
	return nil, errors.New("synthetic open failure")
}

type errProvider struct{}

func (errProvider) Info() provider.Info { return provider.Info{ID: "err", APIVersion: "v0"} }
func (errProvider) Stream(_ context.Context, _ *provider.Request) (provider.EventStream, error) {
	return &errStream{}, nil
}

type errStream struct{}

func (errStream) Next(_ context.Context) (provider.StreamChunk, error) {
	return provider.StreamChunk{}, &providerErr{}
}
func (errStream) Close() error { return nil }

type providerErr struct{}

func (*providerErr) Error() string { return "synthetic provider failure" }

func hasAttr(attrs []attribute.KeyValue, key string) bool {
	for _, a := range attrs {
		if string(a.Key) == key {
			return true
		}
	}
	return false
}

func indexByName(spans tracetest.SpanStubs) map[string]tracetest.SpanStub {
	out := make(map[string]tracetest.SpanStub, len(spans))
	for _, s := range spans {
		out[s.Name] = s
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
