# Starling — Public API

<!-- Prose contracts + navigational overview. The authoritative
signature reference is godoc: https://pkg.go.dev/github.com/jerkeyray/starling -->


Starling is a Go event-sourced LLM agent runtime. Every run is a
hash-chained event log, and every run is byte-for-byte replayable.

This document covers behavioral contracts, extension-point rules, and
stability promises. **Full signatures and field-level docs live on
[pkg.go.dev](https://pkg.go.dev/github.com/jerkeyray/starling)**; this
page does not re-list every field.

---

## 1. Package layout

| Package                            | Role                                                             |
| ---------------------------------- | ---------------------------------------------------------------- |
| `starling`                         | `Agent`, `Config`, `Budget`, `RunResult`, `StepEvent`, CLI verbs |
| `starling/event`                   | Event envelope, `Kind` constants, typed payload helpers          |
| `starling/eventlog`                | `EventLog` interface, in-memory / SQLite / Postgres backends     |
| `starling/provider`                | `Provider` interface, streaming types, aggregated `Response`     |
| `starling/provider/openai`         | OpenAI Chat Completions + every OpenAI-compatible endpoint       |
| `starling/provider/anthropic`      | Anthropic Messages adapter                                       |
| `starling/provider/gemini`         | Google Gemini adapter (Gemini API backend; Vertex AI deferred)   |
| `starling/provider/openrouter`     | OpenRouter adapter (thin wrapper over `provider/openai`)         |
| `starling/tool`                    | `Tool` interface, `Typed[In,Out]` reflective helper              |
| `starling/tool/builtin`            | Small demo tool set (`Fetch`, `ReadFile`)                        |
| `starling/step`                    | Determinism-enforcing primitives: `LLMCall`, `CallTool`, etc.    |
| `starling/replay`                  | Replay-factory plumbing and side-by-side stream                  |
| `starling/inspect`                 | Read-only HTTP inspector (`http.Handler`)                        |

---

## 2. `starling` — root package

### 2.1 `Agent`

`Agent` is a plain struct literal; construction does no work. Fields
are validated on `Run`, not at build time. See godoc for the full field
set.

**Required fields:** `Provider`, `Log`, `Config.Model`.

**Optional:** `Tools`, `Budget` (zero axes are disabled), `Namespace`
(RunID prefix; must not contain `/`), `Metrics`, `Config.Logger`,
`Config.SystemPrompt`, `Config.MaxTurns` (0 = unlimited),
`Config.Params` (`[]byte`, provider-specific params — typically
canonical CBOR).

**Lifecycle methods** (all return terminal events in the log
regardless of how they exit):

- `Run(ctx, goal) (*RunResult, error)` — start a new run.
- `Stream(ctx, goal) (runID, <-chan StepEvent, error)` — start a run
  and observe live events. Setup errors return synchronously; run-time
  errors surface as a terminal `StepEvent` (`Kind` in
  `RunFailed`/`RunCancelled`, `Err` populated) before the channel
  closes. Channel buffer is 64; slow consumers may drop events via the
  underlying eventlog's drop policy, but the channel still closes
  cleanly.
- `Resume(ctx, runID, extraMessage) (*RunResult, error)` — reconstruct
  state from the log and continue a previously-interrupted run.
  `ResumeWith(...ResumeOption)` accepts options:
  - `WithReissueTools(bool)` (default `true`): if `false`, returns
    `ErrPartialToolCall` when the run has an unpaired
    `ToolCallScheduled`.
- `RunReplay(ctx, recorded) error` — re-execute a recorded event
  sequence as an oracle; any divergence wraps `ErrNonDeterminism`.
- `RunReplayInto(ctx, recorded, sink)` — streaming variant; emits into
  `sink` so callers can subscribe via `sink.Stream`.

There is a package-level `Replay(ctx, log, runID, agent)` convenience
wrapper.

### 2.2 Results, events, budget

- `RunResult` — summary: `RunID`, `FinalText`, counts, token/cost
  totals, `Duration`, `TerminalKind`, `MerkleRoot`. Full detail always
  lives in `Log`.
- `StepEvent` — user-facing projection of an `event.Event` for
  `Stream` consumers: `Kind`, `TurnID`, `CallID`, `Text`, `Tool`, `Err`,
  plus `Raw event.Event` for callers that want everything.
- `Budget` — alias of `budget.Budget`. Four axes:
  `MaxInputTokens`, `MaxOutputTokens`, `MaxUSD`, `MaxWallClock`. Zero
  disables an axis. Input is enforced pre-call; output and USD
  mid-stream on every usage chunk; wall-clock via `context.WithDeadline`.

### 2.3 Errors

Sentinel errors (all wrap via `%w` and are discriminated with
`errors.Is`):

- `ErrBudgetExceeded`, `ErrMaxTurnsExceeded`
- `ErrNonDeterminism`, `ErrLogCorrupt` (aliased from `eventlog`)
- `ErrRunNotFound`, `ErrRunAlreadyTerminal`, `ErrRunInUse`,
  `ErrSchemaVersionMismatch`, `ErrPartialToolCall`

Typed errors:

- `ToolError{Name, CallID, Err}` — unrecoverable tool failure.
- `ProviderError{Provider, Code, Err}` — provider-layer failure;
  `Code` is HTTP status when available.

### 2.4 Metrics

`Metrics` is an opt-in Prometheus sink attached via `Agent.Metrics`.
`nil` (the default) is a zero-cost no-op.

- `NewMetrics(reg prometheus.Registerer) *Metrics` — registers every
  collector. Panics on duplicate registration or `nil` registerer.
- `MetricsHandler(g prometheus.Gatherer) http.Handler` — convenience
  over `promhttp.HandlerFor`.

Label cardinality is bounded by the caller's static config (model,
tool name, closed-enum statuses); nothing user-controlled per request
can inflate series count.

### 2.5 CLI helper commands

Every subcommand of the stock `cmd/starling` binary is also exposed as
a root-package helper so dual-mode user binaries can host the same
verbs alongside a real `replay.Factory`. Each constructor returns a
`*<Name>Cmd` whose `Run(args []string) error` parses its own flags.

| Constructor                                   | What it does                                                                              |
| --------------------------------------------- | ----------------------------------------------------------------------------------------- |
| `ValidateCommand() *ValidateCmd`              | Runs `eventlog.Validate` on one run or every run. Read-only.                              |
| `ExportCommand() *ExportCmd`                  | Dumps one run as NDJSON (envelope + typed payload). Read-only.                            |
| `ReplayCommand(factory) *ReplayCmd`           | Headlessly replays one run; prints `OK`/`DIVERGED`. `nil` factory errors with a hint.     |
| `InspectCommand(factory) *InspectCmd`         | Opens the log read-only and serves the inspector over HTTP. `nil` factory = view-only.    |

The stock binary passes `nil` for both factory arguments; users who
want `replay` from the CLI or a live Replay button in the inspector
build a thin `main.go` that dispatches to these helpers with a real
factory wired in. See `examples/m1_hello`.

---

## 3. `starling/event`

`Event` is a tag+payload envelope (`RunID`, `Seq`, `PrevHash`,
`Timestamp`, `Kind`, `Payload`). `Payload` is canonical CBOR on the
wire; typed accessors (`AsRunStarted`, `AsAssistantMessageCompleted`,
one per kind) decode it.

`ToJSON(ev) ([]byte, error)` projects an event to JSON for inspectors
and log dumps — it's a free function, not a method, because the JSON
projection is an external concern. Byte-slice fields are base64 (stdlib
`encoding/json` behavior).

For the full `Kind` list and payload schemas, see `EVENTS.md`.

---

## 4. `starling/eventlog`

### 4.1 Interface

```go
type EventLog interface {
    Append(ctx context.Context, runID string, ev event.Event) error
    Read(ctx context.Context, runID string) ([]event.Event, error)
    Stream(ctx context.Context, runID string) (<-chan event.Event, error)
    Close() error
}
```

**Implementer contract:**

- `Append` MUST reject any event whose `Seq`/`PrevHash` does not
  extend the existing chain for `runID`. This is what makes
  `ErrRunInUse` and `ErrLogCorrupt` meaningful.
- `Stream` MUST deliver events in chain order and MUST close the
  channel when the run reaches a terminal kind or `ctx` is cancelled.
  Slow-subscriber drops are permitted, but the close must be clean.
- `Read` returns the full chain as of the call; it is NOT required to
  include events appended after the read starts.

### 4.2 Shipped backends

- `NewInMemory() EventLog` — process-local, test-grade.
- `NewSQLite(path, opts...) (EventLog, error)` — default durable
  backend; pure Go via `modernc.org/sqlite`. Options include
  `WithReadOnly()` (Append returns `ErrReadOnly`; WAL + change-counter
  checks stay in force so it's safe against a concurrently-writing
  sibling).
- `NewPostgres(db *sql.DB, opts...) (EventLog, error)` — multi-host
  concurrent-writer backend. Caller brings the `*sql.DB` (pgx stdlib or
  `lib/pq`); Starling never imports a driver. Options:
  `WithReadOnlyPG()`, `WithAutoMigratePG()`. `InstallSchema(ctx, db)`
  applies the schema out-of-band. Per-`run_id` serialization is
  enforced with `pg_advisory_xact_lock`.

### 4.3 Optional `RunLister`

```go
type RunLister interface {
    ListRuns(ctx context.Context) ([]RunSummary, error)
}
```

Deliberately **not** part of `EventLog` — write-only/forwarding
backends shouldn't be forced to enumerate. Both shipped backends
satisfy it; callers type-assert when they need it. `RunSummary`
carries `RunID`, `StartedAt`, `LastSeq`, `TerminalKind` (zero when the
run is still in progress), sorted newest-first.

`ErrReadOnly` is the sentinel returned by any read-only handle on
`Append`.

---

## 5. `starling/provider`

### 5.1 Interface

```go
type Provider interface {
    Info() Info
    Stream(ctx context.Context, req *Request) (EventStream, error)
}
```

`Info.ID` and `Info.APIVersion` are recorded into `RunStarted` so
replay can verify the adapter hasn't silently swapped out.

`Request` carries `Model`, `SystemPrompt`, `Messages`, `Tools`, and
caller-controlled `Params` (canonical CBOR bytes). `EventStream.Next`
returns `io.EOF` at end; chunks follow the kinds
`ChunkText`, `ChunkReasoning`, `ChunkToolUseStart`, `ChunkToolUseDelta`,
`ChunkToolUseEnd`, `ChunkUsage`, `ChunkEnd`. See godoc for the full
chunk struct.

**`Response`** is the aggregated outcome returned by `step.LLMCall`
(not by `Provider.Stream` directly). Fields: `Text`, `ToolUses`,
`TurnID`, `StopReason`, `Usage`, `CostUSD`, `RawResponseHash`,
`ProviderReqID`. `TurnID` is minted by `step.LLMCall` and stamped onto
downstream `ToolCall`s for correlation.

**Provider implementer contract:**

- `Stream` MUST emit exactly one `ChunkEnd` at the end of a successful
  stream, carrying `StopReason` and `RawResponseHash` (BLAKE3 over the
  canonical provider response bytes).
- `UsageUpdate.OutputTokens` semantics differ by vendor (OpenAI:
  final-only when `include_usage`; Anthropic: cumulative). `Response`
  normalizes the final value.
- Mid-stream errors MUST propagate through `Next`; the step layer will
  not emit `AssistantMessageCompleted` in that case.

### 5.2 Shipped adapters

- `provider/openai` — covers OpenAI Chat Completions and every
  OpenAI-compatible endpoint (Azure, Groq, Together, OpenRouter,
  Ollama, vLLM, llama.cpp server, DeepSeek, Fireworks, Mistral compat
  mode, etc.). Compat backends are unlocked by `WithBaseURL` — no
  separate adapter per vendor. Options: `WithAPIKey`, `WithBaseURL`,
  `WithHTTPClient`, `WithOrganization`, `WithProviderID`,
  `WithAPIVersion`. Requests always set
  `stream_options.include_usage: true`; mid-stream budget caps fall
  back to a tiktoken local estimate because usage arrives only on the
  terminal chunk.
- `provider/anthropic` — Anthropic Messages API. Options: `WithAPIKey`,
  `WithBaseURL`, `WithHTTPClient`, `WithProviderID`, `WithAPIVersion`
  (default `2023-06-01`).
- `provider/gemini` — Google Gemini API (`generativelanguage.googleapis.com`).
  Options: `WithAPIKey`, `WithBaseURL`, `WithHTTPClient`, `WithProviderID`,
  `WithAPIVersion` (default `v1beta`). Gemini's role model is
  `user`/`model` only; system prompts flow through `Request.SystemPrompt`
  into the dedicated `systemInstruction` field, and tool results are
  delivered as user-role turns carrying a `functionResponse` part.
  Usage arrives only on the terminal stream chunk. Only the Gemini API
  backend is wired today; Vertex AI (OAuth / ADC auth) is a deferred
  follow-up.
- `provider/openrouter` — OpenRouter (`openrouter.ai/api/v1`). Thin
  wrapper over `provider/openai` that sets the base URL, the default
  provider ID (`"openrouter"`), and optional attribution headers
  (`HTTP-Referer`, `X-Title`) via `WithHTTPReferer` / `WithXTitle`.
  Options: `WithAPIKey`, `WithBaseURL`, `WithHTTPReferer`, `WithXTitle`,
  `WithHTTPClient`, `WithProviderID`. Streaming, tool calls, and usage
  accounting are inherited from the OpenAI adapter unchanged.

---

## 6. `starling/tool`

```go
type Tool interface {
    Name() string
    Description() string
    Schema() json.RawMessage
    Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}
```

**Implementer contract:**

- `Name` MUST be stable for the lifetime of the process and unique
  within an `Agent.Tools` slice (the agent rejects duplicates at
  `Run`).
- `Schema` MUST be a JSON Schema describing the `input` shape.
- `Execute` MUST be context-respectful; a panic is caught and
  classified as `panic` in the event log.

`Typed[In, Out any](name, desc, fn) Tool` wraps a typed Go function,
generating `Schema` via reflection on `In` at construction time.

`tool/builtin` ships two demo tools: `Fetch()` (15s HTTP GET, 1 MiB
body cap with silent truncation) and `ReadFile(baseDir)` (path-escape
rejection; 1 MiB hard error).

---

## 7. `starling/step`

The determinism-enforcing primitives. Normally invoked by the default
agent loop; direct callers must be inside a run with a `*Context`
attached via `WithContext`.

- `NewContext(cfg Config) (*Context, error)` — primes the first event
  slot (`seq=1`, `prevHash=nil`). Returns an error (does **not**
  panic) when required config is missing.
- `WithContext(parent, c) context.Context` / `From(ctx) (*Context, bool)` —
  attach and retrieve.
- `LLMCall(ctx, req) (*provider.Response, error)` — one streaming
  completion, emits `TurnStarted → (ReasoningEmitted)* →
  AssistantMessageCompleted`. Enforces input-token budget pre-call and
  output / USD caps mid-stream.
- `CallTool(ctx, ToolCall) (json.RawMessage, error)` — single-tool
  dispatch. Emits `ToolCallScheduled` then either `ToolCallCompleted`
  or `ToolCallFailed`; classifies failures as `tool`, `panic`, or
  `cancelled`. Unknown tools return `ErrToolNotFound`.
- `CallTools(ctx, []ToolCall) ([]ToolResult, error)` — parallel
  dispatch with a semaphore (`Config.MaxParallelTools` or
  `DefaultMaxParallelTools`). Emits Scheduled for every call up front,
  Completed/Failed in real completion order. A failing tool does NOT
  cancel siblings; its error lives in the corresponding
  `ToolResult.Err`. Under `ModeReplay`, calls run sequentially to
  match recorded seq order.
- `Now(ctx)`, `Random(ctx)`, `SideEffect[T](ctx, name, fn)` —
  deterministic wrappers that record on live runs and replay the
  recorded value on replay. `SideEffect.fn` MUST be safe to skip on
  replay.

`Registry` maps tool name → `tool.Tool`. `Names()` is alphabetical so
`RunStarted.ToolRegistryHash` is stable.

Sentinels: `ErrBudgetExceeded`, `ErrToolNotFound`,
`ErrReplayMismatch` (wrapped into `ErrNonDeterminism` at the agent
level).

---

## 8. `starling/replay`

```go
type Factory func(ctx context.Context) (Agent, error)
```

`Factory` builds a fresh agent for one replay attempt — constructed
per session so each replay starts from a clean slate. `Agent` /
`StreamingAgent` are the interfaces the agent must satisfy; the root
`*starling.Agent` satisfies both.

`Stream(ctx, factory, log, runID) (<-chan ReplayStep, error)` runs the
recorded log through a fresh agent and emits one `ReplayStep` per
recorded event (`Index`, `Recorded`, `Produced`, `Diverged`,
`DivergenceReason`). The channel closes on clean finish, divergence,
or ctx cancel.

The replay path is strict and side-effect-free: the recording is the
oracle. There are no public option toggles; a cache-miss-falls-back-to-
live mode is roadmap.

---

## 9. `starling/inspect`

The inspector is a reusable `http.Handler`; the `cmd/starling-inspect`
binary is a shim around `inspect.New`.

- `New(store eventlog.EventLog, opts...) (*Server, error)` — `store`
  must also satisfy `eventlog.RunLister`.
- `Server` implements `http.Handler`; `ReplayEnabled()` reports whether
  a replay factory is wired in.
- `WithReplayer(factory replay.Factory)` — enables the Replay UI.
  Without it the inspector is read-only and the Replay button is
  hidden.
- `WithAuth(Authenticator)` — gates every request (pages, HTMX
  fragments, live-tail SSE, replay endpoints) before the mux. Returning
  false yields 401. `BearerAuth(token)` is the one built-in;
  comparison is constant-time and an empty token panics (pass `nil` to
  `WithAuth` for no auth).

---

## 10. Worked example

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/jerkeyray/starling"
    "github.com/jerkeyray/starling/event"
    "github.com/jerkeyray/starling/eventlog"
    "github.com/jerkeyray/starling/provider/openai"
    "github.com/jerkeyray/starling/tool"
    "github.com/jerkeyray/starling/tool/builtin"
)

func main() {
    ctx := context.Background()

    prov, err := openai.New(openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
    if err != nil { log.Fatal(err) }
    // For an OpenAI-compatible endpoint (Groq, Ollama, vLLM, ...):
    //   openai.New(openai.WithAPIKey(k),
    //       openai.WithBaseURL("https://api.groq.com/openai/v1"))

    agent := &starling.Agent{
        Provider: prov,
        Tools:    []tool.Tool{builtin.Fetch()},
        Log:      eventlog.NewInMemory(),
        Budget:   &starling.Budget{MaxOutputTokens: 2000, MaxUSD: 0.10},
        Config: starling.Config{
            Model:        "gpt-4o-mini",
            SystemPrompt: "You are a helpful assistant.",
            MaxTurns:     10,
        },
    }

    // Blocking API.
    result, err := agent.Run(ctx, "What time is it in Tokyo?")
    if err != nil { log.Fatal(err) }
    fmt.Printf("run %s: %s\n", result.RunID, result.FinalText)

    // Or: stream events live.
    runID, events, err := agent.Stream(ctx, "do the thing")
    if err != nil { log.Fatal(err) }
    _ = runID
    for se := range events {
        switch se.Kind {
        case event.KindAssistantMessageCompleted:
            fmt.Println("assistant:", se.Text)
        case event.KindToolCallScheduled:
            fmt.Printf("tool: %s (%s)\n", se.Tool, se.CallID)
        case event.KindRunFailed, event.KindRunCancelled:
            fmt.Println("terminal:", se.Err)
        }
    }

    // Verify the run is reproducible.
    if err := starling.Replay(ctx, agent.Log, result.RunID, agent); err != nil {
        log.Fatal(err)
    }
}
```

See `examples/` for more complete binaries, including dual-mode
`inspect` wiring.

---

## 11. Design constraints

- **No sealed interfaces.** Every interface is user-implementable.
- **No hidden globals, no `Init()`, no hub object.**
- **`context.Context` first on every exported function.**
- **All errors wrap with `%w`.** `errors.Is` / `errors.As` are the only
  discriminators users need.
- **Zero-value agents are usable** once `Provider`, `Log`, and
  `Config.Model` are set. Everything else has a sane default.
- **Event log is the source of truth.** `RunResult`, `Stream`,
  metrics, and the inspector are all derived views.

---

## 12. Non-goals

- No prompt templating — write Go strings.
- No vector stores — write a `Tool`.
- No multi-agent coordination.
- No chain/flow DSL.
- No HTTP server wrapper around `Agent`.
- No built-in retry policies (tools and providers wrap themselves).
- No hub/registry global.
