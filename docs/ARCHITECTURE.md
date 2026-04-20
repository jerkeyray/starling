# Starling — Architecture

> Package layout, data flow, and module boundaries for v0.
> Last updated: apr 2026

---

## 1. Module

Go module: `github.com/jerkeyray/starling`
Go version minimum: 1.23 (for `iter.Seq2` in `Agent.Stream`)

---

## 2. Package layout

```
starling/
├── go.mod
├── agent.go                    # root package: Agent, Run*, Config
├── error.go                    # sentinel errors
├── event/
│   ├── event.go                # Event type, type constants
│   ├── types.go                # payload structs per event kind
│   ├── hash.go                 # BLAKE3 chain helpers
│   └── encoding.go             # CBOR canonical marshal/unmarshal
├── eventlog/
│   ├── eventlog.go             # EventLog interface
│   ├── memory.go               # in-memory implementation (M1)
│   ├── sqlite.go               # SQLite-backed (M2, default durable)
│   ├── postgres.go             # Postgres-backed (M2, for existing infra)
│   └── conformance.go          # shared test suite every backend must pass
├── provider/
│   ├── provider.go             # Provider, Request, Response, EventStream interfaces
│   ├── openai/                 # M1 — OpenAI + OpenAI-compatible via WithBaseURL
│   │   ├── openai.go           # OpenAI Chat Completions adapter
│   │   └── stream.go           # SSE delta → StreamChunk mapping
│   └── anthropic/              # M3 — deferred
│       ├── anthropic.go        # Anthropic adapter
│       └── stream.go           # streaming adapter + token counting
├── tool/
│   ├── tool.go                 # Tool interface
│   ├── typed.go                # tool.Typed[In, Out] generic helper
│   └── builtin/
│       ├── fetch.go            # HTTP fetch tool (demo)
│       └── readfile.go         # file read tool (demo)
├── step/
│   ├── step.go                 # Now, Random, SideEffect, LLMCall, CallTool
│   └── context.go              # run context handle
├── budget/
│   ├── budget.go               # Budget struct, enforcer
│   └── tokens.go               # per-provider token counting
├── replay/
│   ├── replay.go               # state-faithful replay
│   └── cache.go                # re-execution cache (opt-in)
├── internal/
│   └── cborenc/                # canonical CBOR helpers (private)
└── examples/
    └── code-review/
        └── main.go             # demo agent (M4)
```

### 2.1 Public root package (≤ 10 exported types)

1. `Agent` — configured agent, owns Run/Resume/Replay/Stream methods
2. `Config` — agent configuration
3. `Budget` — cost budget (re-exported from `budget`)
4. `RunResult` — terminal run state
5. `StepEvent` — user-facing event-stream item (from `Agent.Stream`)
6. `ErrBudgetExceeded`, `ErrNonDeterminism`, `ErrRunNotFound` — sentinel errors
7. `ToolError`, `ProviderError` — typed error wrappers

Everything else (event types, Provider interface, Tool interface, EventLog interface) lives in subpackages. Users import what they need.

---

## 3. Data flow — a single turn

```
┌─────────────────────────────────────────────────────────────────────┐
│                          Agent.Run(ctx, goal)                       │
└─────────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────┐
│  1. step.NewRun(agent, goal)                                        │
│     → allocate run_id, open new log, emit RunStarted                │
└─────────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────┐
│  2. Agent loop (deterministic code)                                 │
│                                                                     │
│     for !done {                                                     │
│         resp := step.LLMCall(ctx, request)         ─┐ Command       │
│         if resp.ToolCalls == nil { done = true }    │ → Events      │
│         outputs := step.CallTools(ctx, calls)       │               │
│         append tool outputs to context             ─┘               │
│     }                                                               │
└─────────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────┐
│  3. step.LLMCall                                                 │
│     a. emit TurnStarted event                                       │
│     b. check budget: can we afford input_tokens?                    │
│     c. open streaming call to Provider                              │
│     d. budget watchdog goroutine consumes usage updates             │
│     e. on budget trip → cancel ctx → emit BudgetExceeded            │
│     f. on success → emit AssistantMessageCompleted                  │
│     g. optional: emit ReasoningEmitted if provider returns it       │
└─────────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────┐
│  4. step.CallTools                                               │
│     a. for each tool call: emit ToolCallScheduled                   │
│     b. errgroup.WithContext → run all tools in parallel             │
│     c. per tool: emit ToolCallCompleted | ToolCallFailed            │
│     d. aggregate outputs for next LLM turn                          │
└─────────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────┐
│  5. terminal                                                        │
│     emit RunCompleted | RunFailed | RunCancelled                    │
│     compute Merkle root over events, include in terminal event      │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 4. Core boundaries

### 4.1 Agent loop boundary

The agent loop is a pure function `(ctx, step.Context, input) → commands`. It runs inside `step.NewRun`. Rules:

- No direct I/O, time, randomness
- No goroutines (the runtime spawns them for parallel tool calls)
- All side effects via `step.*` functions
- Must be safe to call repeatedly with the same inputs (replay re-invokes it)

The default loop is a ReAct implementation in `agent.go`. Advanced users can supply a custom loop function via `Config.Loop` (rarely needed; M3+).

### 4.2 Step boundary

`step` is the only package that writes to the log. Rules:

- Every event goes through `step.append(ctx, event)` which computes `prev_hash` and invokes `EventLog.Append`
- Agent-loop code never sees an `EventLog` — only the `step.Context` handle
- During replay, `step.LLMCall` and `step.CallTool` read recorded outputs instead of calling out; the log is never written

### 4.3 Provider boundary

Providers implement `Provider`:

```go
type Provider interface {
    Stream(ctx context.Context, req *Request) (EventStream, error)
}

type EventStream interface {
    Next(ctx context.Context) (StreamChunk, error)  // io.EOF terminates
    Close() error
}

type StreamChunk struct {
    Kind      ChunkKind        // Text | ToolUseStart | ToolUseDelta | ToolUseEnd | Usage | End
    Text      string
    ToolUse   *ToolUseChunk
    Usage     *UsageUpdate     // final-only for OpenAI-family (include_usage: true); cumulative for Anthropic
    RawResponseHash []byte     // set on End chunks
}
```

The provider emits normalized chunks. Token counting, budget enforcement, and event emission happen in `runtime`, not in the provider. This keeps providers simple and enforcement uniform.

### 4.4 Log boundary

`EventLog` is pluggable:

```go
type EventLog interface {
    Append(ctx context.Context, runID string, ev event.Event) error
    Read(ctx context.Context, runID string) ([]event.Event, error)
    Stream(ctx context.Context, runID string) (<-chan event.Event, error)
    Close() error
}
```

Shipped: `eventlog.NewInMemory()` (M1) and `eventlog.NewSQLite(path)` (M2, the default durable backend). A Postgres backend (`NewPostgres`) is on the roadmap but not yet implemented. Custom backends (Pebble, BadgerDB, S3, etc.) plug in via the interface.

The log is write-once from `step`'s perspective — `step` appends, never updates. Reads are allowed from anywhere (inspect-while-running, replay, audit).

### 4.5 Tool boundary

```go
type Tool interface {
    Name() string
    Description() string
    Schema() json.RawMessage
    Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}
```

Tools are non-deterministic (like Temporal Activities). Runtime records their args (in `ToolCallScheduled`) and results (in `ToolCallCompleted`); tool code is free to call networks, read files, etc. On replay, tools are not invoked — recorded results are returned directly.

---

## 5. Control flow: Run vs Resume vs Replay

### 5.1 Run

Fresh run. New `run_id`. Empty log. Agent loop executes normally; runtime writes events.

### 5.2 Resume

Existing `run_id`; log has events; terminal event absent (or user wants to continue past `RunCompleted`). `step` replays the log through the agent loop to rehydrate context, then continues making real LLM/tool calls.

### 5.3 Replay

Existing `run_id`; log is complete. `step` runs the agent loop with `ReplayMode = true`. LLM calls return recorded `AssistantMessageCompleted` payloads; tool calls return recorded results. Each time the loop issues a command, `step` diffs against the next recorded event. Mismatch → `ErrNonDeterminism`.

Replay produces a `RunResult` identical to the original (in state-faithful mode). In re-execution mode, LLM calls go through a response cache; cache miss → call provider.

---

## 6. Concurrency model

- **One agent run at a time within a `Run()` call.** Internal goroutines exist only for parallel tool calls (`errgroup`) and budget watchdog.
- **Multiple concurrent runs share an Agent.** `Agent` is stateless; two goroutines can call `agent.Run(ctx, "a")` and `agent.Run(ctx, "b")` safely.
- **EventLog implementations handle their own concurrency.** `log.Memory` uses `sync.RWMutex`; SQLite uses WAL mode + busy-timeout; Postgres uses per-row locking on `(run_id, seq)`.
- **Budget watchdog**: a single goroutine per streaming LLM call. Consumes `StreamChunk`s, updates local token counter, calls `cancel()` when budget trips. Joined on stream close.

### 6.1 Multi-tenant layout

The SQLite backend keys events on `(run_id, seq)` with no separate
tenant column. Two tenants picking the same raw RunID would collide.
The `Agent.Namespace` field (added in M3) sidesteps this without a
schema change: when set, every event written by that agent carries
`RunID = Namespace + "/" + ULID`, and all lookups use the prefixed
form. One SQLite file can host many tenants safely as long as each
agent is constructed with a distinct `Namespace`. Empty namespace
preserves pre-M3 behavior.

### 6.2 Performance / backpressure

Event appends are **synchronous**. `step.emit` blocks until the log
backend returns:

- `eventlog.Memory` — in-memory append + fan-out to stream
  subscribers; effectively non-blocking.
- `eventlog.SQLite` — `BEGIN IMMEDIATE` + single-row INSERT +
  `COMMIT`. Under WAL mode this is sub-millisecond on healthy local
  disk, but a slow or contended disk stalls the emitting goroutine,
  which in turn stalls the agent turn that called `step.Now` / tool
  dispatch / `step.LLMCall`.

This is a deliberate tradeoff: the core audit-trail guarantee is
"emit returned nil means the event is on disk." An async write
buffer would weaken that guarantee (buffered events lost on crash),
so none is shipped by default.

Practical implications:

- Tune for fast local storage. Network filesystems (NFS, EFS)
  behave poorly under WAL mode.
- If the log write itself fails, `step.Now` / `step.Random` panic
  with a `non-replayable` message rather than silently continuing
  with a broken hash chain.
- A long-running agent on a slow disk is detectable via OTel span
  durations — `agent.turn` latency that exceeds LLM+tool latency by
  much is a disk-stall signal.

Future work: an opt-in bounded write buffer (drops the
write-through guarantee for throughput) is a follow-up, not a
default.

---

## 6.3 Observability

Three independent layers, pick whichever subset you need:

1. **Event log** — the source of truth. Every emission lands as a
   typed `event.Event` with `(RunID, Seq, PrevHash, Timestamp, Kind,
   Payload)`. This is the audit trail; nothing else here can replace
   it. Read it via `EventLog.Read(ctx, runID)` after the run, or
   stream via `EventLog.Stream(ctx, runID)` while the run is live.

2. **`log/slog`** — structured side-channel trace. Set
   `Agent.Config.Logger`; every record carries `run_id`, plus
   `turn_id` / `call_id` / `attempt` where relevant. Levels:
   - `Debug` — turn boundaries.
   - `Info` — `run started`, `run completed`, `run cancelled`.
   - `Warn` — budget trips, transient tool retries.
   - `Error` — `run failed` (terminal).

   Defaults to `slog.Default()` when nil; pass an `io.Discard`-backed
   handler to silence library output entirely.

3. **OpenTelemetry** — distributed-trace integration. The library
   uses the global `otel.Tracer` provider; without an SDK configured
   you pay only the no-op tracer indirection. Span tree:

   ```
   agent.run
   └── agent.turn
       ├── agent.llm_call
       └── agent.tool_call (one per attempt; retries add Attempt attr)
   ```

   Span errors mirror the terminal event kind. Use OTel for "what
   takes the time?" investigations; use slog for "what happened
   when?"; use the event log for "what *exactly* happened?"

---

## 7. Error handling

- All errors wrap with `%w`. Users use `errors.Is`/`errors.As`.
- Runtime-level errors are recorded as `RunFailed` events with the error string. The error returned to the caller is the same error; the event is the audit trail.
- Tool errors are recorded as `ToolCallFailed` events and bubble up as `ToolError`.
- Provider errors are recorded in the chunk stream and turn into `TurnFailed` events; they bubble up as `ProviderError`.
- `context.Canceled` is propagated cleanly: if the budget watchdog cancels, the resulting error is `ErrBudgetExceeded`; if the user cancels, it's `context.Canceled`.

---

## 8. Dependencies (planned)

Minimal third-party surface:

- `github.com/fxamacker/cbor/v2` — canonical CBOR
- `github.com/zeebo/blake3` — BLAKE3 hashing
- `github.com/openai/openai-go` — M1 provider (OpenAI + OpenAI-compatible APIs via `WithBaseURL`)
- `github.com/pkoukk/tiktoken-go` — local token counting for OpenAI-family budgets (M1)
- `github.com/anthropics/anthropic-sdk-go` — M3 provider (deferred)
- `golang.org/x/sync/errgroup` — parallel tool execution
- `go.opentelemetry.io/otel` — tracing (optional, interface-only dep in core)

No dependency is allowed in the root package except the stdlib. Subpackages pull in their narrow deps.

---

## 9. Versioning

- v0.x releases — API may change freely
- v1.0 — frozen public API; event schema frozen; replay compatibility guaranteed going forward
- Event schema version recorded in `RunStarted` event (`schema_version: 1`). Replayer refuses to replay a log with a schema version it doesn't understand.

---

## 10. What lives where — quick reference

| Concern | Package | Notes |
|---|---|---|
| Define an Agent | `starling` (root) | `Agent`, `Config`, `Budget` |
| Run/Resume/Replay | `starling` (root) | methods on `*Agent` |
| Event types | `starling/event` | `Event`, payload structs, encoding |
| Event log backends | `starling/eventlog` | `EventLog` interface + implementations |
| LLM providers | `starling/provider/*` | OpenAI + OpenAI-compatible (via `WithBaseURL`) in M1; Anthropic, Gemini in M3 |
| Tool interface | `starling/tool` | `Tool`, `Typed[In, Out]` helper |
| Side-effect helpers | `starling/step` | `Now`, `Random`, `SideEffect`, `LLMCall`, `CallTool` |
| Budget enforcement | `starling/budget` | `Budget`, token counters |
| Replay machinery | `starling/replay` | replayer + response cache |
| Demo agent | `examples/code-review` | shipped in M4 |
