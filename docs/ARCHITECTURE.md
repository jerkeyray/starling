# Starling — Architecture

> Current package layout, runtime data flow, and subsystem boundaries.
> Last updated: Apr 2026

---

## 1. Module

Go module: `github.com/jerkeyray/starling`
Minimum Go version: 1.26+

Starling is a Go agent runtime built around an append-only event log.
Every run is recorded as typed events with a BLAKE3 hash chain and a
Merkle-rooted terminal event. The core product shape is:

- run an agent
- persist every step as events
- validate / inspect that log later
- replay the run against the same agent wiring and detect divergence

---

## 2. Package layout

```
starling/
├── agent.go                # Agent, run loop, terminal/result assembly
├── config.go               # Config and Budget re-export
├── errors.go               # root sentinel / typed errors
├── stream.go               # Agent.Stream live API
├── resume.go               # Resume / ResumeWith
├── replay_api.go           # package-level Replay helper
├── inspect_command.go      # dual-mode inspector CLI entrypoint
├── *_command.go            # validate/export/replay CLI helpers
├── event/
│   ├── event.go            # envelope + Kind
│   ├── types.go            # typed payload structs
│   ├── encoding.go         # canonical CBOR encode/decode
│   ├── hash.go             # event hash helpers
│   └── payload_json.go     # event.ToJSON projection
├── eventlog/
│   ├── eventlog.go         # EventLog, RunLister, RunSummary
│   ├── memory.go           # in-memory backend
│   ├── sqlite.go           # durable local backend
│   ├── postgres.go         # durable multi-host backend
│   └── validate.go         # hash-chain / merkle validation
├── provider/
│   ├── provider.go         # Provider interface and normalized stream types
│   ├── openai/             # OpenAI + compatible endpoints
│   ├── anthropic/          # Anthropic Messages API
│   ├── gemini/             # Gemini API backend
│   └── openrouter/         # thin wrapper over provider/openai
├── tool/
│   ├── tool.go             # Tool interface
│   ├── typed.go            # tool.Typed helper
│   └── builtin/            # demo/reference tools
├── step/
│   ├── context.go          # run-scoped execution context
│   ├── llm.go              # step.LLMCall
│   ├── tools.go            # step.CallTool / CallTools
│   └── step.go             # deterministic helpers (Now, Random, SideEffect)
├── replay/
│   ├── replay.go           # verify/replay plumbing
│   └── stream.go           # side-by-side replay streaming
├── inspect/
│   ├── server.go           # inspector handler and routing
│   ├── handlers.go         # HTTP handlers
│   ├── live.go             # live-tail SSE
│   ├── replay.go           # replay UI/session plumbing
│   └── ui/                 # embedded templates + static assets
└── examples/
    ├── m1_hello/
    └── m4_inspector_demo/
```

### 2.1 Public root package

The root package is intentionally small and operator-facing:

- `Agent`, `Config`, `Budget`
- `RunResult`, `StepEvent`
- `Replay`, `Resume`, `Stream`
- CLI helpers: `ValidateCommand`, `ExportCommand`, `ReplayCommand`,
  `InspectCommand`
- errors: `ErrBudgetExceeded`, `ErrNonDeterminism`, `ErrRunNotFound`,
  typed wrappers like `ToolError` and `ProviderError`

Most extension points live in subpackages: `provider`, `tool`,
`eventlog`, `inspect`, `replay`, and `step`.

---

## 3. Runtime data flow

### 3.1 `Agent.Run`

`(*Agent).Run(ctx, goal)` is the blocking API:

1. Validate agent configuration.
2. Mint a RunID (`Namespace + "/" + ULID` when namespaced).
3. Build a `step.Context` over the configured `EventLog`.
4. Emit `RunStarted`.
5. Enter the ReAct loop:
   - call `step.LLMCall`
   - if tool calls were planned, execute them with `step.CallTools`
   - append tool results to the next LLM turn's message history
6. Emit one terminal event:
   - `RunCompleted`
   - `RunFailed`
   - `RunCancelled`
7. Re-read the run from the log and materialize `RunResult`.

`RunResult` is a convenience summary. The event log remains the source
of truth.

### 3.2 `Agent.Stream`

`(*Agent).Stream(ctx, goal)` is the live API:

- subscribe to `EventLog.Stream` before `RunStarted` is emitted
- run the same runtime as `Run`
- project raw events into `StepEvent`
- close the returned channel on terminal event or context cancellation

The API is channel-based. It does not use `iter.Seq` or `iter.Seq2`.

### 3.3 `Resume` and `Replay`

- `Resume` rebuilds run state from recorded events and then continues
  with real provider/tool calls.
- `Replay` verifies a completed run against the current agent wiring.
- `RunReplay` and `RunReplayInto` are the lower-level agent methods
  used by the root helper and the inspector replay UI.

Resume is for continuation after interruption. Replay is for proving
determinism and surfacing divergence.

---

## 4. Core boundaries

### 4.1 Agent boundary

`Agent` owns orchestration, not storage or provider details. It wires
the configured `Provider`, `Tools`, `EventLog`, budgets, metrics, and
logger into a `step.Context`, then drives the loop.

The default loop is the shipped ReAct loop in `agent.go`. There is no
public `Config.Loop` hook in the current API.

### 4.2 Step boundary

`step` is the runtime layer that turns actions into events. It owns:

- `LLMCall`
- `CallTool` / `CallTools`
- deterministic helpers: `Now`, `Random`, `SideEffect`
- replay-aware behavior for those helpers

This is the package that appends runtime events through `EventLog`.
The agent loop does not compute hashes or mutate the log directly.

### 4.3 Provider boundary

Providers implement a normalized streaming interface:

```go
type Provider interface {
    Info() Info
    Stream(ctx context.Context, req *Request) (EventStream, error)
}
```

Adapters translate vendor-specific streaming APIs into normalized
chunks such as text, reasoning, tool-use fragments, usage, and end.
Budget enforcement and event emission happen in Starling's runtime, not
inside provider adapters.

Shipped adapters today:

- `provider/openai`
- `provider/anthropic`
- `provider/gemini`
- `provider/openrouter`

OpenAI-compatible endpoints are still primarily served through
`provider/openai` with `WithBaseURL`.

### 4.4 Event log boundary

The event log contract is intentionally small:

```go
type EventLog interface {
    Append(ctx context.Context, runID string, ev event.Event) error
    Read(ctx context.Context, runID string) ([]event.Event, error)
    Stream(ctx context.Context, runID string) (<-chan event.Event, error)
    Close() error
}
```

Built-in backends:

- `eventlog.NewInMemory()`
- `eventlog.NewSQLite(path, opts...)`
- `eventlog.NewPostgres(db, opts...)`

Enumeration is intentionally separate via `eventlog.RunLister` so
forwarding/write-only backends are not forced to implement listing.

#### `Stream` semantics

`EventLog.Stream` is a history-plus-live subscription. It is designed
for inspection and live observers, not as a total-order replication
protocol.

Current contract:

- subscribers receive stored events and then newly appended events
- channel closes on context cancellation, log close, or subscriber
  overflow
- under concurrent appends, strict history-then-live ordering is not
  guaranteed on every backend

Consumers that need exact monotonic processing must track the highest
`Seq` seen and discard duplicates or out-of-order replays with
`ev.Seq <= lastSeen`. The inspector live-tail path is the reference
consumer.

### 4.5 Inspect boundary

`inspect.Server` is a reusable `http.Handler` over an
`eventlog.EventLog` that also satisfies `eventlog.RunLister`.

It is intentionally read-mostly:

- browse runs
- inspect event payloads
- validate hash chains
- live-tail in-progress runs
- optionally stream replay steps when a replay factory is wired in

The standalone `cmd/starling-inspect` binary is a thin read-only SQLite
shim around this library. Replay-from-UI is enabled only when a caller
ships its own dual-mode binary with `starling.InspectCommand(factory)`.

---

## 5. Concurrency and durability

### 5.1 Agent concurrency

- multiple goroutines may share one `*Agent`
- each run has its own `step.Context`
- parallelism inside a run is limited to tool dispatch and provider
  streaming / budget enforcement

### 5.2 Event durability

Event appends are synchronous. When an emit call returns nil, that
event has been accepted by the configured backend.

Tradeoff:

- better audit guarantees
- more sensitivity to slow disks / slow database commits

This is deliberate. Starling does not ship an async write buffer in
the default path.

### 5.3 Multi-tenant layout

`Agent.Namespace` prefixes RunIDs so one backing log can safely host
many tenants or applications without schema changes:

```text
<namespace>/<ulid>
```

Routing, replay, inspector paths, and CLI helpers all treat `/` inside
RunIDs as normal and suffix-parse from the right where needed.

---

## 6. Observability

Three layers exist side by side:

1. Event log
   The canonical audit trail.
2. `log/slog`
   Side-channel operational trace via `Config.Logger`. Opt-in: nil is
   silent; pass `slog.New(...)` to enable. Replay divergences and
   dropped event-log subscribers are always logged via `slog.Default()`
   regardless.
3. OpenTelemetry
   Spans around run / turn / LLM / tool boundaries.

Metrics are opt-in through `Agent.Metrics` and the Prometheus helpers
in the root package.

---

## 7. Error model

- runtime failures become terminal events in the log
- errors returned to callers still carry the typed/sentinel values
- replay mismatches surface as `ErrNonDeterminism`
- read-only logs return `eventlog.ErrReadOnly` on append attempts
- validation failures wrap `eventlog.ErrLogCorrupt`

The log and the returned Go error are meant to agree: one is the audit
trail, the other is the direct control-flow signal to the caller.

---

## 8. Dependencies

External dependencies are scoped by subsystem, not centralized into one
"engine" package.

Examples:

- `fxamacker/cbor` for canonical CBOR encoding
- `zeebo/blake3` for event hashing
- `openai-go/v3`, `anthropic-sdk-go`, and `google.golang.org/genai`
  for provider adapters
- `modernc.org/sqlite` for the SQLite backend
- `prometheus/client_golang` for metrics
- OpenTelemetry packages for tracing

The root package is not stdlib-only today; some core runtime behavior
depends on packages like ULID, BLAKE3, and OpenTelemetry.

---

## 9. Quick reference

| Concern | Package | Notes |
|---|---|---|
| Agent runtime | `starling` | `Agent`, `Config`, `Budget`, commands |
| Event schema | `starling/event` | envelope, payloads, hashing, JSON projection |
| Event storage | `starling/eventlog` | in-memory, SQLite, Postgres, validation |
| Providers | `starling/provider/*` | OpenAI, Anthropic, Gemini, Bedrock, OpenRouter |
| Tools | `starling/tool` | interface + `tool.Typed` |
| Deterministic runtime ops | `starling/step` | LLM/tool/time/random helpers |
| Replay plumbing | `starling/replay` | verify + side-by-side replay stream |
| Inspector UI | `starling/inspect` | reusable handler and embedded UI |
| Example app | `examples/m1_hello` | canonical dual-mode binary |
| Inspector demo | `examples/m4_inspector_demo` | local UI/demo data |
