# Starling — Prometheus metrics

Starling exposes an opt-in Prometheus metrics surface covering run
lifecycle, provider calls, tool invocations, event-log appends, and
budget trips. The collector set is fixed and cardinality-bounded:
every label is a closed enum (status, error_type, model, tool,
kind, axis), so scrape output stays stable as a user's run volume
scales.

Nil `Agent.Metrics` (the default) disables the entire pipeline. No
goroutines, no maps, no counters — just a nil check at every call
site. If you haven't wired `NewMetrics`, you pay nothing.

## Quickstart

```go
import (
    "net/http"

    starling "github.com/jerkeyray/starling"
    "github.com/prometheus/client_golang/prometheus"
)

reg := prometheus.NewRegistry()
metrics := starling.NewMetrics(reg)

agent := &starling.Agent{
    Provider: p,
    Tools:    tools,
    Log:      log,
    Config:   cfg,
    Metrics:  metrics,
}

http.Handle("/metrics", starling.MetricsHandler(reg))
go http.ListenAndServe(":9090", nil)
```

`NewMetrics(reg)` panics on a nil registerer or on duplicate
registration — misuse surfaces at boot, not silently at scrape
time. Prefer `prometheus.NewRegistry()` over
`prometheus.DefaultRegisterer` so collector lifetimes stay tied to
your process state.

## The metric set

Every metric name is prefixed `starling_`. Durations are in seconds
(the Prometheus convention). `none` appears in `error_type` for
non-error terminals so the label is never empty — dashboards can
sum across it without special-casing.

### Run lifecycle

| Name | Type | Labels | Description |
|---|---|---|---|
| `starling_runs_started_total` | Counter | — | Total runs started. |
| `starling_runs_in_flight` | Gauge | — | Runs currently executing. |
| `starling_run_terminal_total` | Counter | `status`, `error_type` | Terminal outcomes. `status` ∈ {ok, error, cancelled, budget}. `error_type` ∈ {none, budget, max_turns, tool, provider, internal}. |
| `starling_run_duration_seconds` | Histogram | `status` | Wall-clock run duration, bucketed. |

### Provider

| Name | Type | Labels | Description |
|---|---|---|---|
| `starling_provider_calls_total` | Counter | `model`, `status` | `Provider.Stream` invocations. `status` ∈ {ok, error, cancelled}. |
| `starling_provider_call_duration_seconds` | Histogram | `model` | Stream open → EOF (or error). |
| `starling_provider_tokens_total` | Counter | `model`, `type` | Tokens reported by the provider. `type` ∈ {prompt, completion}. No sample is recorded for a zero value. |

### Tool

| Name | Type | Labels | Description |
|---|---|---|---|
| `starling_tool_calls_total` | Counter | `tool`, `status`, `error_type` | Tool invocations per attempt. `error_type` ∈ {none, tool, panic, cancelled}. |
| `starling_tool_call_duration_seconds` | Histogram | `tool` | Per-attempt wall-clock. |

### Event log

| Name | Type | Labels | Description |
|---|---|---|---|
| `starling_eventlog_appends_total` | Counter | `kind`, `status` | Every `Append` call. `kind` is `event.Kind.String()`. |
| `starling_eventlog_append_duration_seconds` | Histogram | `kind` | Narrow buckets (50 µs – 100 ms) so tail spikes stand out. |

### Budget

| Name | Type | Labels | Description |
|---|---|---|---|
| `starling_budget_exceeded_total` | Counter | `axis` | Budget trips. `axis` ∈ {input_tokens, output_tokens, cost_usd, wall_clock}. |

## Example queries

Runs/second by terminal status:

```promql
sum by (status) (rate(starling_run_terminal_total[5m]))
```

Provider p99 latency, per model:

```promql
histogram_quantile(0.99,
  sum by (le, model) (rate(starling_provider_call_duration_seconds_bucket[5m])))
```

Tool error rate (any error class):

```promql
sum(rate(starling_tool_calls_total{status="error"}[5m]))
  /
sum(rate(starling_tool_calls_total[5m]))
```

In-flight runs, now:

```promql
starling_runs_in_flight
```

## Label discipline

Every label value is drawn from a fixed enum in the codebase:

- `status` is produced by `classifyRunError` / `classifyToolError`
  / `providerCallStatus`; never the raw error string.
- `model` comes straight from `Agent.Config.Model`; bounded by your
  deployment.
- `tool` comes from the tool's `Name()`; bounded by your registry.
- `kind` is `event.Kind.String()`; ~15 values.

Cardinality is bounded by construction. There is no opt-out for
adding a user-defined label — that's the deal you trade for
stability.

## Bucket choices

Durations use `ExponentialBucketsRange(0.005, 120, 12)` for run /
provider / tool histograms — covers agent runs from "sub-10 ms
tool" to "two-minute complex run" with twelve buckets. The
eventlog histogram uses a tighter `ExponentialBucketsRange(0.00005,
0.1, 10)` band because that's where disk trouble shows up as tail
latency; a wide-band histogram would smear the signal.

These buckets are part of the API. Changing them later would break
dashboards, so they're pinned.

## What's not here (yet)

- **Grafana dashboard JSON.** Tracked under
  `examples/grafana/` — coming once a couple of users report back
  what panels they actually look at.
- **OpenTelemetry metrics bridge.** Starling already emits OTel
  traces (`internal/obs`); a metrics bridge alongside this
  Prometheus surface is a separate roadmap item.
- **Cost-per-minute.** Needs a pricing table we don't want to
  maintain in core. Derive from `starling_provider_tokens_total` +
  a separate model-pricing recording rule.
- **Inspector metrics.** The inspector's session / SSE counters
  are trivial additions once there's a reason.
