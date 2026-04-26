// Package main: incident-triage example.
//
// A multi-tool agent that takes an incident description, fetches a
// metric, searches past incidents, and (optionally) escalates. The
// run records every step into the event log so the inspector can
// replay and audit it.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/budget"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/step"
	"github.com/jerkeyray/starling/tool"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const defaultDB = "./incident_triage.db"
const namespace = "incident-triage"

// ----------------------------------------------------------------------
// tools
// ----------------------------------------------------------------------

type metricLookupIn struct {
	Service string `json:"service"`
	Metric  string `json:"metric"`
}
type metricLookupOut struct {
	Service string  `json:"service"`
	Metric  string  `json:"metric"`
	Value   float64 `json:"value"`
}

func metricLookupTool() tool.Tool {
	return tool.Typed(
		"metric_lookup",
		"Fetch the latest value of a service metric. Returns the numeric value.",
		func(_ context.Context, in metricLookupIn) (metricLookupOut, error) {
			if in.Service == "" || in.Metric == "" {
				return metricLookupOut{}, fmt.Errorf("service and metric are required")
			}
			return metricLookupOut{
				Service: in.Service,
				Metric:  in.Metric,
				Value:   syntheticMetric(in.Service, in.Metric),
			}, nil
		},
	)
}

func syntheticMetric(service, metric string) float64 {
	// Deterministic-ish blend so a few well-known combinations look
	// realistic; real deployments swap this for a Prometheus query.
	switch metric {
	case "error_rate":
		return 0.07
	case "p99_latency_ms":
		return 412.5
	case "qps":
		return 1850
	}
	return float64(len(service)+len(metric)) / 10
}

type incidentSearchIn struct {
	Query string `json:"query"`
}
type incidentSearchOut struct {
	Matches []pastIncident `json:"matches"`
}
type pastIncident struct {
	ID      string `json:"id"`
	Date    string `json:"date"`
	Summary string `json:"summary"`
}

var pastIncidents = []pastIncident{
	{ID: "INC-1024", Date: "2025-11-12", Summary: "checkout-api 5xx surge after rate-limit config push"},
	{ID: "INC-1188", Date: "2026-01-30", Summary: "checkout-api latency spike from saturated DB pool"},
	{ID: "INC-1232", Date: "2026-03-14", Summary: "search-api 5xx tied to embedding cache eviction"},
}

func incidentSearchTool() tool.Tool {
	return tool.Typed(
		"incident_search",
		"Search the historical incident log for entries matching the query.",
		func(_ context.Context, in incidentSearchIn) (incidentSearchOut, error) {
			q := strings.ToLower(strings.TrimSpace(in.Query))
			if q == "" {
				return incidentSearchOut{}, fmt.Errorf("query is required")
			}
			out := incidentSearchOut{}
			for _, p := range pastIncidents {
				if strings.Contains(strings.ToLower(p.Summary), q) {
					out.Matches = append(out.Matches, p)
				}
			}
			return out, nil
		},
	)
}

type escalateIn struct {
	Channel string `json:"channel"`
	Summary string `json:"summary"`
}
type escalateOut struct {
	Acknowledged bool   `json:"acknowledged"`
	Channel      string `json:"channel"`
}

func escalateTool(notify func(channel, summary string) error) tool.Tool {
	return tool.Typed(
		"escalate",
		"Page a human on-call channel with a one-line incident summary.",
		func(ctx context.Context, in escalateIn) (escalateOut, error) {
			if in.Channel == "" || in.Summary == "" {
				return escalateOut{}, fmt.Errorf("channel and summary are required")
			}
			// Route every external side effect through step.SideEffect
			// so the result is captured in the event log and the next
			// replay returns the recorded value instead of paging again.
			ack, err := step.SideEffect(ctx, "escalate", func() (escalateOut, error) {
				if notify != nil {
					if err := notify(in.Channel, in.Summary); err != nil {
						return escalateOut{}, err
					}
				}
				return escalateOut{Acknowledged: true, Channel: in.Channel}, nil
			})
			if err != nil {
				return escalateOut{}, err
			}
			return ack, nil
		},
	)
}

// ----------------------------------------------------------------------
// canned provider — used in CI replay tests and as a no-key default
// ----------------------------------------------------------------------

// cannedProvider replays a fixed sequence of provider streams. Mirrors
// the test scaffolding in the root package so the example can run in
// CI without API keys.
type cannedProvider struct {
	scripts [][]provider.StreamChunk
	idx     int
}

func (p *cannedProvider) Info() provider.Info {
	return provider.Info{ID: "canned", APIVersion: "v0"}
}

func (p *cannedProvider) Stream(_ context.Context, _ *provider.Request) (provider.EventStream, error) {
	if p.idx >= len(p.scripts) {
		return nil, errors.New("canned: scripts exhausted")
	}
	s := &cannedStream{chunks: p.scripts[p.idx]}
	p.idx++
	return s, nil
}

type cannedStream struct {
	chunks []provider.StreamChunk
	pos    int
}

func (s *cannedStream) Next(_ context.Context) (provider.StreamChunk, error) {
	if s.pos >= len(s.chunks) {
		return provider.StreamChunk{}, io.EOF
	}
	c := s.chunks[s.pos]
	s.pos++
	return c, nil
}

func (s *cannedStream) Close() error { return nil }

// scriptedTriage returns the canned 2-turn conversation the example
// runs by default. Turn 1: assistant calls metric_lookup +
// incident_search. Turn 2: assistant calls escalate then summarises.
func scriptedTriage() [][]provider.StreamChunk {
	// Marshal via map so JSON keys come out alphabetically — replay's
	// CBOR→JSON round-trip emits sorted keys, so the canned script
	// must too or the assistant's recorded tool-use args won't
	// byte-match the replay reconstruction.
	mkArgs := func(v any) string {
		b, _ := json.Marshal(toSortedMap(v))
		return string(b)
	}
	return [][]provider.StreamChunk{
		// Turn 1
		{
			{Kind: provider.ChunkText, Text: "Pulling metrics and history."},
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "mc1", Name: "metric_lookup"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "mc1", ArgsDelta: mkArgs(metricLookupIn{Service: "checkout-api", Metric: "error_rate"})}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "mc1"}},
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "is1", Name: "incident_search"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "is1", ArgsDelta: mkArgs(incidentSearchIn{Query: "checkout-api"})}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "is1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 120, OutputTokens: 32}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
		// Turn 2
		{
			{Kind: provider.ChunkText, Text: "Escalating to oncall."},
			{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "esc1", Name: "escalate"}},
			{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "esc1", ArgsDelta: mkArgs(escalateIn{Channel: "#oncall-checkout", Summary: "checkout-api error_rate=0.07; matches INC-1024 / INC-1188."})}},
			{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "esc1"}},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 220, OutputTokens: 18}},
			{Kind: provider.ChunkEnd, StopReason: "tool_use"},
		},
		// Turn 3 — final summary, no tools.
		{
			{Kind: provider.ChunkText, Text: "checkout-api error_rate is 0.07 (above SLO). Past incidents INC-1024 and INC-1188 are similar — likely rate-limit config or DB pool. Escalated to #oncall-checkout."},
			{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 260, OutputTokens: 38}},
			{Kind: provider.ChunkEnd, StopReason: "stop"},
		},
	}
}

// ----------------------------------------------------------------------
// agent construction
// ----------------------------------------------------------------------

type buildOpts struct {
	dbPath    string
	useCanned bool
	notify    func(channel, summary string) error
	metrics   *starling.Metrics
}

func buildAgent(_ context.Context, opts buildOpts) (*starling.Agent, error) {
	prov, model, err := pickProvider(opts.useCanned)
	if err != nil {
		return nil, err
	}

	log, err := openLog(opts.dbPath)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}

	cfg := starling.Config{
		Model:                  model,
		MaxTurns:               6,
		AppVersion:             os.Getenv("APP_VERSION"),
		RequireRawResponseHash: false,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if os.Getenv("DEBUG") == "1" {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	a := &starling.Agent{
		Provider: prov,
		Tools: []tool.Tool{
			metricLookupTool(),
			incidentSearchTool(),
			escalateTool(opts.notify),
		},
		Log:       log,
		Config:    cfg,
		Namespace: namespace,
		Budget: &budget.Budget{
			MaxOutputTokens: 4_000,
			MaxUSD:          0.25,
			MaxWallClock:    2 * time.Minute,
		},
	}
	if opts.metrics != nil {
		a.Metrics = opts.metrics
	}
	return a, nil
}

// pickProvider returns a canned provider when forced or when no API
// key is present, so `go run ./examples/incident_triage` works out of
// the box. Real providers can be selected via PROVIDER=openai|anthropic.
func pickProvider(forceCanned bool) (provider.Provider, string, error) {
	if forceCanned || os.Getenv("PROVIDER") == "canned" || os.Getenv("OPENAI_API_KEY") == "" {
		return &cannedProvider{scripts: scriptedTriage()}, "canned-incident-triage", nil
	}
	// Real-provider wiring is intentionally minimal here; see the
	// m1_hello example for a fuller multi-provider switch.
	return nil, "", fmt.Errorf("set PROVIDER=canned to run without an API key, or wire a real provider in pickProvider()")
}

// openLog defaults to SQLite. Set DATABASE_URL=postgres://… to use
// Postgres; the schema is created/migrated on open.
func openLog(path string) (eventlog.EventLog, error) {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		return eventlog.NewPostgres(db, eventlog.WithAutoMigratePG())
	}
	if path == "" {
		path = defaultDB
	}
	return eventlog.NewSQLite(path)
}

// ----------------------------------------------------------------------
// canned-mode validation aid
// ----------------------------------------------------------------------

// summarizeRun is a tiny helper used by run-mode and the smoke test
// to print a one-line status after the agent finishes.
func summarizeRun(res *starling.RunResult) string {
	if res == nil {
		return "no result"
	}
	return fmt.Sprintf("run=%s terminal=%s turns=%d cost=$%.4f",
		res.RunID, res.TerminalKind, res.TurnCount, res.TotalCostUSD)
}

// loadValidate is a small helper exposed for the smoke test.
func loadValidate(log eventlog.EventLog, runID string) error {
	evs, err := log.Read(context.Background(), runID)
	if err != nil {
		return err
	}
	return eventlog.Validate(evs)
}

// firstRunStarted returns the seq of the RunStarted event, used in
// tests to assert event ordering deterministically.
func firstRunStarted(events []event.Event) (uint64, bool) {
	for _, ev := range events {
		if ev.Kind == event.KindRunStarted {
			return ev.Seq, true
		}
	}
	return 0, false
}

// toSortedMap round-trips v through JSON into a map[string]any. The
// resulting map marshals back to JSON with alphabetical keys, which
// matches the cbor→map→json reconstruction the replay provider uses.
func toSortedMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
