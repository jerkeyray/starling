# incident_triage — realistic Starling example

A 3-turn incident-triage agent built end-to-end against the Starling
runtime: multiple tools, a SideEffect-bound external escalation, the
budget caps, the inspector, headless replay verification, Resume,
Prometheus metrics, OpenTelemetry traces, and a Postgres-or-SQLite
backend.

The `run` mode ships with a canned conversation so you can exercise
every code path without an API key. Swap in a real provider via
`pickProvider` to drive it with a live LLM.

## Quick start

```sh
go run ./examples/incident_triage run
go run ./examples/incident_triage inspect ./incident_triage.db
```

Add `OTEL=1` to dump traces, `METRICS_ADDR=:9090` to expose
`/metrics`, `DATABASE_URL=postgres://…` to swap SQLite for Postgres.

## What the agent does

Goal: *"checkout-api error_rate spiked at 14:00 UTC; triage and
escalate if necessary."*

Tools:

| Tool              | Behavior                                                    |
|-------------------|-------------------------------------------------------------|
| `metric_lookup`   | Returns a synthetic metric value keyed by `(service,metric)`. |
| `incident_search` | Returns past incidents whose summary contains the query substring. |
| `escalate`        | Pages a channel via `step.SideEffect` (recorded in the log). |

Turn flow (canned):

1. `metric_lookup(checkout-api, error_rate)` + `incident_search("checkout-api")` (parallel).
2. `escalate(#oncall-checkout, summary)` (records a `SideEffectRecorded`).
3. Final summary — no tools.

## Replay regression test

`agent_test.go` records a fresh run end-to-end and re-executes it via
`replay.Verify`. Drift in the canned script, tools, or step helpers
surfaces as an `ErrNonDeterminism`-wrapped error. The test runs in CI
without secrets — the canned provider is deterministic.

```sh
go test ./examples/incident_triage/
```

## Postgres mode

```sh
DATABASE_URL=postgres://localhost/starling_dev?sslmode=disable \
  go run ./examples/incident_triage run

# inspect Postgres logs the same way (the inspector opens via DSN
# wiring in your dual-mode binary; the stock starling-inspect binary
# is SQLite-only for now)
```

## Resume

```sh
# Crash the run mid-tool, then resume. Any pending tool calls get
# fresh CallIDs and the run continues from the seam.
go run ./examples/incident_triage resume ./incident_triage.db <runID>
```

## Headless replay

```sh
go run ./examples/incident_triage replay ./incident_triage.db <runID>
```

Exits non-zero on drift. Useful as a CI smoke check after agent or
tool edits.

## Metrics

When `METRICS_ADDR` is set, the example serves Prometheus metrics on
that address.

```sh
METRICS_ADDR=:9090 go run ./examples/incident_triage run
curl localhost:9090/metrics | grep starling_
```

## Tracing

`OTEL=1` wires a stdout exporter that pretty-prints every span. Drop
in any OTLP exporter for production.

```sh
OTEL=1 go run ./examples/incident_triage run
```

Span tree:

```
agent.run
└── agent.turn × 3
    ├── provider.stream
    └── step.tool × N
```

## Docker

```dockerfile
FROM golang:1.23 AS build
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/triage ./examples/incident_triage

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/triage /triage
USER 65532:65532
ENTRYPOINT ["/triage"]
CMD ["run"]
```

Compose with a Postgres service and pipe `DATABASE_URL` in via env.

## What this example demonstrates

- Multi-tool dispatch under `step.CallTools`.
- `step.SideEffect` for non-deterministic side effects with replay safety.
- Budget caps (`MaxOutputTokens`, `MaxUSD`, `MaxWallClock`).
- `Namespace` so run IDs sort under one prefix.
- Replay regression as a CI gate.
- Optional Postgres backend behind the same `EventLog` interface.
- Prometheus metrics + OpenTelemetry traces.

## What it intentionally does not do

- Talk to a real LLM by default (canned provider keeps CI hermetic).
- Hash the raw provider response (the canned provider is your own
  bytes; in production set `Config.RequireRawResponseHash = true`).
- Persist past incidents to a real store (the slice is a deterministic
  fixture).
