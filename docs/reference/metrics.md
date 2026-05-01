# Reference: metrics and traces

Every Prometheus metric Starling emits and every OpenTelemetry span
it opens. Source of truth: [`metrics.go`](../../metrics.go) and
[`internal/obs/trace.go`](../../internal/obs/trace.go).

Metrics are **opt-in**: the runtime emits nothing unless
`Agent.Metrics` is set. Traces are also opt-in, ride the global
OTel provider, and are no-op when no SDK is configured.

## Wiring

```go
import (
    "github.com/prometheus/client_golang/prometheus"
    starling "github.com/jerkeyray/starling"
)

reg := prometheus.NewRegistry()
metrics := starling.NewMetrics(reg)

agent := &starling.Agent{
    // ...
    Metrics: metrics,
}

// Serve to Prometheus.
http.Handle("/metrics", starling.MetricsHandler(reg))
```

`NewMetrics(nil)` returns nil; the agent and step layer call
through nil-safe receivers, so a nil `Metrics` is the cheapest
disabled state.

## Prometheus metrics

| Name | Type | Labels | Notes |
| --- | --- | --- | --- |
| `starling_runs_started_total` | counter | — | One bump per `Agent.Run` entry. |
| `starling_runs_in_flight` | gauge | — | Increments at run entry, decrements at exit. |
| `starling_run_terminal_total` | counter | `status`, `error_type` | `status` ∈ {`ok`, `error`, `cancelled`, `budget`}; `error_type` is `none` on `ok`/`cancelled`, else the `RunFailed.ErrorType` value. |
| `starling_run_duration_seconds` | histogram | `status` | 12 exponential buckets, 5 ms → 120 s. |
| `starling_provider_calls_total` | counter | `model`, `status` | One per `Provider.Stream()` open; `status` ∈ {`ok`, `error`, `cancelled`}. |
| `starling_provider_call_duration_seconds` | histogram | `model` | Stream open to EOF or error. |
| `starling_provider_tokens_total` | counter | `model`, `type` | `type` ∈ {`prompt`, `completion`}. |
| `starling_tool_calls_total` | counter | `tool`, `status`, `error_type` | One per attempt; `error_type` ∈ {`tool`, `panic`, `cancelled`, `other`} on error, `none` on ok. |
| `starling_tool_call_duration_seconds` | histogram | `tool` | Per attempt. |
| `starling_eventlog_appends_total` | counter | `kind`, `status` | `kind` is `event.Kind.String()`. |
| `starling_eventlog_append_duration_seconds` | histogram | `kind` | Tighter buckets (50 µs → 100 ms) — disk-trouble tail spikes are visible here. |
| `starling_budget_exceeded_total` | counter | `axis` | `axis` ∈ {`input_tokens`, `output_tokens`, `cost_usd`, `wall_clock`}. |

All metrics use seconds for time and align with Prometheus
naming conventions. Histogram buckets are documented in
[`metrics.go`](../../metrics.go) for re-tuning.

## OpenTelemetry spans

Tracer name (instrumentation scope):
`github.com/jerkeyray/starling`.

| Span name | Where | Notable attributes |
| --- | --- | --- |
| `agent.run` | Root span over `Agent.Run` / `Resume`. | `run_id` |
| `agent.turn` | One per ReAct turn. | `turn_id`, `turn` (1-based int) |
| `agent.llm_call` | One per provider streaming call (`step.LLMCall`). | `model` |
| `agent.tool_call` | One per tool invocation **attempt** (retries get their own span). | `tool_name`, `call_id`, `attempt` |

Common attribute keys (also used in `slog`):

| Attribute | Source |
| --- | --- |
| `run_id` | `obs.AttrRunID` |
| `turn_id` | `obs.AttrTurnID` |
| `call_id` | `obs.AttrCallID` |
| `tool_name` | `obs.AttrToolName` |
| `attempt` | `obs.AttrAttempt` |
| `kind` | `obs.AttrKind` (terminal/event kind in log lines) |
| `dur_ms` | `obs.AttrDurMs` (wall-clock in `slog` lines) |

Spans are marked errored via `obs.SetSpanError` at every error
return so the trace view tracks failures correctly.

## Wiring an OTel exporter

The runtime depends only on `go.opentelemetry.io/otel` types and
the global tracer provider. Set up your exporter in `main`:

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

exp, _ := stdouttrace.New(stdouttrace.WithPrettyPrint())
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
otel.SetTracerProvider(tp)
defer tp.Shutdown(ctx)
```

After that, every `Agent.Run` produces a tree rooted at
`agent.run`. `examples/incident_triage/main.go` sets up the same
shape end-to-end.

## See also

- [`metrics.go`](../../metrics.go) — the source of truth for
  Prometheus names and helper methods.
- [`internal/obs/trace.go`](../../internal/obs/trace.go) — the
  source of truth for span names and attribute keys.
- [`examples/incident_triage`](../../examples/incident_triage) —
  full Prometheus + OTel wiring.
