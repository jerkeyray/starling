# Starling — Public API (v0 draft)

> Concrete signatures for every exported type. Working "hello agent" examples.
> Last updated: apr 2026

---

## 1. Root package — `github.com/jerkeyray/starling`

### 1.1 Agent

```go
type Agent struct {
    Provider provider.Provider
    Tools    []tool.Tool
    Log      log.EventLog
    Budget   *Budget          // optional
    Config   Config
}

type Config struct {
    Model         string
    SystemPrompt  string
    Params        cbor.RawMessage  // provider-specific params (temperature, top_p, max_tokens, ...)
    MaxTurns      int              // 0 = unlimited (not recommended)
    Tracer        trace.TracerProvider  // OTel, optional
}
```

Methods:

```go
// Run starts a new agent run. Returns the final result plus the run ID for later replay.
func (a *Agent) Run(ctx context.Context, goal string) (*RunResult, error)

// Resume loads an in-progress run from the log and continues it.
// Optionally injects a new user message.
func (a *Agent) Resume(ctx context.Context, runID string, extraMessage string) (*RunResult, error)

// Replay re-runs the agent loop against a recorded log. In state-faithful mode (default),
// LLM and tool calls return recorded results. Returns the reproduced RunResult.
func (a *Agent) Replay(ctx context.Context, runID string, opts ...ReplayOption) (*RunResult, error)

// Stream starts a new run and emits StepEvents as they occur. Caller drains the channel.
// The run continues even if the caller stops draining (events still written to log).
func (a *Agent) Stream(ctx context.Context, goal string) (runID string, events <-chan StepEvent, err error)
```

### 1.2 RunResult

```go
type RunResult struct {
    RunID          string
    FinalText      string
    TurnCount      int
    ToolCallCount  int
    TotalCostUSD   float64
    InputTokens    int64
    OutputTokens   int64
    Duration       time.Duration
    TerminalKind   event.Kind   // RunCompleted | RunFailed | RunCancelled
    MerkleRoot     []byte
}
```

### 1.3 StepEvent

User-facing view of an event. Narrower than the raw `event.Event`.

```go
type StepEvent struct {
    Kind    event.Kind
    TurnID  string
    CallID  string
    Text    string        // for AssistantMessageCompleted or token-stream chunks
    Tool    string        // for tool call events
    Err     error         // non-nil on Failed kinds
    Raw     event.Event   // full event for consumers who want it
}
```

### 1.4 Budget

Re-exported from `budget` for convenience.

```go
type Budget = budget.Budget

// Budget enforcement limits. Zero = no limit on that axis.
type Budget struct {
    MaxInputTokens  int64
    MaxOutputTokens int64
    MaxUSD          float64
    MaxWallClock    time.Duration
}
```

### 1.5 Errors

```go
var (
    ErrBudgetExceeded    = errors.New("starling: budget exceeded")
    ErrNonDeterminism    = errors.New("starling: non-determinism detected during replay")
    ErrRunNotFound       = errors.New("starling: run not found in log")
    ErrLogCorrupt        = errors.New("starling: log failed validation")
    ErrMaxTurnsExceeded  = errors.New("starling: max turns exceeded")
)

type ToolError struct {
    Name string
    Err  error
}

func (e *ToolError) Error() string { ... }
func (e *ToolError) Unwrap() error { return e.Err }

type ProviderError struct {
    Provider string
    Code     int
    Err      error
}

func (e *ProviderError) Error() string { ... }
func (e *ProviderError) Unwrap() error { return e.Err }
```

### 1.6 Replay options

```go
type ReplayOption func(*replayConfig)

// ReplayReexecute re-runs LLM and tool calls during replay. Requires a response cache
// (provided via option or internally constructed from the log). Cache miss = live call.
func ReplayReexecute() ReplayOption

// ReplayStrict fails immediately on non-determinism (default is strict).
func ReplayStrict(b bool) ReplayOption
```

---

## 2. `starling/event`

```go
type Event struct {
    RunID     string
    Seq       uint64
    PrevHash  []byte
    Timestamp int64
    Kind      Kind
    Payload   cbor.RawMessage
}

type Kind uint8

const (
    KindRunStarted Kind = 1
    // ... (see EVENTS.md)
)

// Typed helpers for each kind:
func (e Event) AsRunStarted() (RunStarted, error)
func (e Event) AsTurnStarted() (TurnStarted, error)
func (e Event) AsAssistantMessageCompleted() (AssistantMessageCompleted, error)
func (e Event) AsToolCallScheduled() (ToolCallScheduled, error)
// ... one per kind

// ToJSON projects an event's payload to JSON. The shape mirrors the
// canonical CBOR exactly — every payload struct's `cbor:"..."` tags
// are mirrored as `json:"..."`. Intended for inspectors, log dumps,
// and tooling that wants a human-readable view; the canonical wire
// format remains CBOR. Byte-slice fields (PrevHash, raw response
// hashes, CBOR-encoded tool args) appear as base64 — that's how
// `encoding/json` handles `[]byte`.
//
// Function, not a method: the JSON projection is an external concern
// and a free function keeps `Event` itself decoder-shaped.
func ToJSON(ev Event) ([]byte, error)
```

---

## 3. `starling/eventlog`

```go
type EventLog interface {
    Append(ctx context.Context, runID string, ev event.Event) error
    Read(ctx context.Context, runID string) ([]event.Event, error)
    Stream(ctx context.Context, runID string) (<-chan event.Event, error)
    Close() error
}

// In-memory implementation (M1).
func NewInMemory() EventLog

// SQLite-backed implementation (M2). Default durable backend. Pure Go via modernc.org/sqlite.
func NewSQLite(path string, opts ...SQLiteOption) (EventLog, error)

// Postgres-backed implementation (M2). For users with existing Postgres infra.
// Caller supplies the *sql.DB so connection pooling / migrations stay in their control.
func NewPostgres(db *sql.DB) (EventLog, error)

// SQLiteOption tunes SQLite open behavior.
type SQLiteOption func(*sqliteConfig)

// WithReadOnly opens the database with `?mode=ro`. Append always
// returns ErrReadOnly. Intended for inspector-style tools that must
// not mutate the audit log they are inspecting. Crucially does NOT
// pass `immutable=1`: a read-only handle remains correct against a
// database that another Starling process is actively writing to,
// because WAL + change-counter checks stay in effect.
func WithReadOnly() SQLiteOption

// ErrReadOnly is returned by Append on a handle opened WithReadOnly.
var ErrReadOnly = errors.New("eventlog: log is read-only")

// RunLister is an OPTIONAL interface for backends that can enumerate
// the runs they hold. It is intentionally NOT part of EventLog so
// write-only / forwarding backends are not forced to support it.
// Both built-in backends (NewInMemory, NewSQLite) satisfy it; type-
// assert when you need to enumerate:
//
//     lister, ok := log.(eventlog.RunLister)
//     if !ok { ... }
//     summaries, err := lister.ListRuns(ctx)
type RunLister interface {
    ListRuns(ctx context.Context) ([]RunSummary, error)
}

// RunSummary is the minimum needed to render a runs-list view —
// inspectors, dashboards, scripts that triage failed runs. Sorted
// newest-first by StartedAt.
type RunSummary struct {
    RunID        string
    StartedAt    time.Time  // wall-clock time of the first event
    LastSeq      uint64     // doubles as event count: events are 1-indexed
    TerminalKind event.Kind // 0 if the run never terminated (still in progress)
}
```

---

## 4. `starling/provider`

```go
type Provider interface {
    Info() Info
    Stream(ctx context.Context, req *Request) (EventStream, error)
}

// Info is consumed by the agent loop to populate RunStarted.ProviderID and
// RunStarted.APIVersion. Adapters return a stable identifier (e.g. "openai",
// "groq") and an API-version string (e.g. "v1", "2023-06-01").
type Info struct {
    ID         string
    APIVersion string
}

type Request struct {
    Model        string
    SystemPrompt string
    Messages     []Message
    Tools        []ToolDefinition
    Params       cbor.RawMessage   // caller-controlled provider-native params
}

type Role string

const (
    RoleSystem    Role = "system"
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleTool      Role = "tool"
)

type Message struct {
    Role       Role
    Content    string
    ToolUses   []ToolUse    // when Role=assistant and the model planned tool calls
    ToolResult *ToolResult  // when Role=tool
}

type ToolDefinition struct {
    Name        string
    Description string
    Schema      json.RawMessage
}

type ToolUse struct {
    CallID string
    Name   string
    Args   json.RawMessage
}

type ToolResult struct {
    CallID  string
    Content string
    IsError bool
}

type EventStream interface {
    Next(ctx context.Context) (StreamChunk, error)  // returns io.EOF at end
    Close() error
}

type StreamChunk struct {
    Kind            ChunkKind
    Text            string
    ToolUse         *ToolUseChunk
    Usage           *UsageUpdate
    StopReason      string        // set on End chunks
    RawResponseHash []byte        // set on End chunks (BLAKE3 of canonical provider bytes)
    ProviderReqID   string
}

type ToolUseChunk struct {
    CallID    string
    Name      string  // set on ChunkToolUseStart only
    ArgsDelta string  // set on ChunkToolUseDelta; concatenate across chunks for full JSON
}

// Note: ChunkReasoning is part of the surface for future adapters (Anthropic
// extended thinking, o-series reasoning traces). The M1 OpenAI Chat Completions
// adapter does not emit it — OpenAI's public streaming API surfaces no
// reasoning deltas.

type ChunkKind uint8

const (
    ChunkText ChunkKind = iota + 1
    ChunkReasoning
    ChunkToolUseStart
    ChunkToolUseDelta
    ChunkToolUseEnd
    ChunkUsage
    ChunkEnd
)

type UsageUpdate struct {
    InputTokens       int64   // final value (OpenAI: last chunk; Anthropic: message_start)
    OutputTokens      int64   // final-only for OpenAI-family (include_usage: true); cumulative for Anthropic
    CacheReadTokens   int64
    CacheCreateTokens int64
}

// Response is the aggregated outcome of a streaming completion,
// returned by step.LLMCall. ToolUses carries Args as provider-reported
// JSON bytes — step.CallTool accepts those verbatim via ToolCall.Args.
type Response struct {
    Text            string
    ToolUses        []ToolUse
    StopReason      string
    Usage           UsageUpdate
    CostUSD         float64
    RawResponseHash []byte
    ProviderReqID   string
}
```

### 4.1 `starling/provider/openai` (M1)

Adapter for the OpenAI Chat Completions API and every OpenAI-compatible endpoint (Azure OpenAI, Groq, Together, OpenRouter, Ollama, vLLM, LM Studio, llama.cpp server, DeepSeek, Fireworks, Anyscale, Mistral compat mode, …). Compatibility providers are unlocked by `WithBaseURL` — no separate adapter per vendor.

```go
// Options
func New(opts ...Option) (provider.Provider, error)

type Option func(*config)

func WithAPIKey(key string) Option
func WithBaseURL(url string) Option         // e.g. "https://api.groq.com/openai/v1"
func WithHTTPClient(c *http.Client) Option
func WithOrganization(org string) Option    // OpenAI-only; ignored by compat providers
func WithProviderID(id string) Option       // overrides Info().ID (default "openai"); useful when a compat backend wants a distinct identifier in the event log
func WithAPIVersion(v string) Option        // overrides Info().APIVersion (default "v1")
```

Requests always set `stream_options: {include_usage: true}` so the final SSE chunk carries token usage. Budget enforcement on OpenAI-family providers is best-effort: because usage only arrives on the terminal chunk, mid-stream caps fall back to a tiktoken-based local estimate of emitted output tokens.

### 4.2 `starling/provider/anthropic` (M3 — deferred)

```go
// Options
func New(opts ...Option) (provider.Provider, error)

type Option func(*config)

func WithAPIKey(key string) Option
func WithHTTPClient(c *http.Client) Option
func WithBaseURL(url string) Option
func WithAPIVersion(v string) Option  // default: "2023-06-01"
```

---

## 5. `starling/tool`

```go
type Tool interface {
    Name() string
    Description() string
    Schema() json.RawMessage
    Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

// Generic helper. Users write typed Go functions; Typed wraps them as Tools.
// Schema is generated via reflection on In at construction time.
func Typed[In, Out any](name, description string, fn func(context.Context, In) (Out, error)) Tool
```

Usage:

```go
type WeatherInput struct {
    City string `json:"city"`
}

type WeatherOutput struct {
    Temperature float64 `json:"temperature"`
    Condition   string  `json:"condition"`
}

weatherTool := tool.Typed(
    "get_weather",
    "Get the current weather for a city.",
    func(ctx context.Context, in WeatherInput) (WeatherOutput, error) {
        return WeatherOutput{Temperature: 72, Condition: "sunny"}, nil
    },
)
```

### 5.1 `starling/tool/builtin` (M1 demo set)

```go
// Fetches an HTTP URL with a 15s timeout. Returns {status, body}; body is
// capped at 1 MiB (oversize bodies are truncated without error).
func Fetch() tool.Tool

// Reads a local file under baseDir. Rejects paths that escape baseDir
// (via "..", absolute paths, or symlinks). Returns contents as string.
// Files larger than 1 MiB return an error rather than silently truncating.
func ReadFile(baseDir string) tool.Tool
```

---

## 6. `starling/step`

The determinism-enforcing API. Normally invoked from the default agent loop; advanced users calling it directly must ensure they're inside an agent run.

```go
// Context is the opaque per-run state. Owns the event log, run ID,
// provider/tool/budget dependencies, and the hash-chain cursor every
// emitted event advances. Safe for concurrent use across the step
// helpers.
type Context struct { /* opaque */ }

// Config bundles everything NewContext needs. Log and RunID are
// required; Provider is required for LLMCall; Tools is required for
// CallTool. Budget is optional — a zero-valued BudgetConfig disables
// the pre-call input-token cap.
type Config struct {
    Log      eventlog.EventLog
    RunID    string
    Provider provider.Provider
    Tools    *Registry
    Budget   BudgetConfig
}

// BudgetConfig is the subset of budget caps step enforces. The full
// Budget struct (wall-clock, USD, output-token caps) arrives with T11.
type BudgetConfig struct {
    MaxInputTokens int64 // 0 means unlimited
}

// Sentinel errors callers (and the agent loop) route on with errors.Is.
var ErrBudgetExceeded = errors.New("step: budget exceeded")
var ErrToolNotFound   = errors.New("step: tool not found")

// NewContext primes a Context to emit the first event of a run (seq=1,
// prevHash=nil). Panics if cfg.Log is nil or cfg.RunID is empty.
// Provider and Tools are validated lazily by LLMCall and CallTool.
func NewContext(cfg Config) *Context

// RunID returns the run identifier this Context was constructed with.
func (c *Context) RunID() string

// WithContext attaches c to parent so downstream step calls can reach it.
func WithContext(parent context.Context, c *Context) context.Context

// From extracts the Context previously attached via WithContext. Returns
// (nil, false) when no Context is attached.
func From(ctx context.Context) (*Context, bool)

// Now returns the current time, recorded to the log on live runs and replayed on replay.
func Now(ctx context.Context) time.Time

// Random returns a uniform random uint64 from the run's deterministic source.
func Random(ctx context.Context) uint64

// SideEffect runs fn once, records the return value, and returns recorded value on replay.
// fn must be safe to skip on replay.
func SideEffect[T any](ctx context.Context, name string, fn func() (T, error)) (T, error)

// LLMCall performs one streaming completion against the configured
// Provider, enforcing Budget.MaxInputTokens pre-call and emitting
// TurnStarted → (ReasoningEmitted)* → AssistantMessageCompleted.
// Returns ErrBudgetExceeded (and emits BudgetExceeded) when the
// pre-call estimate exceeds the cap. On mid-stream error, returns
// the error unchanged without emitting AssistantMessageCompleted.
func LLMCall(ctx context.Context, req *provider.Request) (*provider.Response, error)

// ToolCall describes a tool invocation. CallID carries the LLM's
// assigned identifier so the full Planned → Scheduled → Completed
// chain links back to AssistantMessageCompleted; empty CallID is
// auto-minted as a ULID. TurnID is required for correlation.
type ToolCall struct {
    CallID string
    TurnID string
    Name   string
    Args   json.RawMessage
}

// CallTool dispatches the requested tool against the Registry in the
// Context, emits ToolCallScheduled before invocation and either
// ToolCallCompleted or ToolCallFailed after, and classifies failures
// into {"tool", "panic", "cancelled"} per EVENTS.md §3.8. Unknown
// tools produce Scheduled+Failed and return ErrToolNotFound.
func CallTool(ctx context.Context, call ToolCall) (json.RawMessage, error)

// Registry maps tool names to tool.Tool implementations for a single
// run. Constructed once at run start and shared across goroutines.
type Registry struct { /* opaque */ }

func NewRegistry(tools ...tool.Tool) *Registry
func (r *Registry) Get(name string) (tool.Tool, bool)
func (r *Registry) Names() []string // alphabetical; stable for RunStarted.ToolRegistryHash

// Deferred to M2: CallTools (parallel errgroup variant). Design preserved
// in plan docs; not shipped in M1.
```

---

## 7. Worked example — hello agent

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "time"

    "github.com/jerkeyray/starling"
    "github.com/jerkeyray/starling/eventlog"
    "github.com/jerkeyray/starling/provider/openai"
    "github.com/jerkeyray/starling/tool"
    "github.com/jerkeyray/starling/tool/builtin"
)

type TimeInput struct{}
type TimeOutput struct {
    ISO8601 string `json:"iso8601"`
}

func main() {
    ctx := context.Background()

    prov, err := openai.New(openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
    if err != nil { log.Fatal(err) }
    // For an OpenAI-compatible provider (Groq, Ollama, vLLM, …), add WithBaseURL:
    //   openai.New(openai.WithAPIKey(k), openai.WithBaseURL("https://api.groq.com/openai/v1"))

    currentTime := tool.Typed(
        "current_time",
        "Return the current time in ISO8601.",
        func(ctx context.Context, _ TimeInput) (TimeOutput, error) {
            return TimeOutput{ISO8601: time.Now().UTC().Format(time.RFC3339)}, nil
        },
    )

    agent := &starling.Agent{
        Provider: prov,
        Tools:    []tool.Tool{currentTime, builtin.Fetch()},
        Log:      eventlog.NewInMemory(),
        Budget: &starling.Budget{
            MaxOutputTokens: 2000,
            MaxUSD:          0.10,
        },
        Config: starling.Config{
            Model:        "gpt-4o-mini",
            SystemPrompt: "You are a helpful assistant.",
            MaxTurns:     10,
        },
    }

    result, err := agent.Run(ctx, "What time is it in Tokyo?")
    if err != nil {
        log.Fatalf("run failed: %v", err)
    }

    fmt.Printf("Run %s: %s\n", result.RunID, result.FinalText)
    fmt.Printf("Cost: $%.4f, %d turns, %d tool calls\n",
        result.TotalCostUSD, result.TurnCount, result.ToolCallCount)

    // Later: inspect every event
    events, _ := agent.Log.Read(ctx, result.RunID)
    for _, ev := range events {
        fmt.Printf("  %d %s\n", ev.Seq, ev.Kind)
    }

    // Later: replay
    replayed, err := agent.Replay(ctx, result.RunID)
    if err != nil { log.Fatal(err) }
    fmt.Println("Replayed:", replayed.FinalText) // identical
}
```

---

## 8. Streaming example (M3)

```go
runID, events, err := agent.Stream(ctx, "Summarize the Go 1.23 release notes.")
if err != nil { log.Fatal(err) }

for ev := range events {
    switch ev.Kind {
    case event.KindAssistantMessageCompleted:
        fmt.Printf("[turn %s] %s\n", ev.TurnID, ev.Text)
    case event.KindToolCallScheduled:
        fmt.Printf("[tool %s] calling %s\n", ev.CallID, ev.Tool)
    case event.KindBudgetExceeded:
        fmt.Printf("[!] budget exceeded: %v\n", ev.Err)
    }
}
```

---

## 9. Design constraints reflected in the API

- **Root package exports exactly 10 types.** Counted: `Agent`, `Config`, `Budget`, `RunResult`, `StepEvent`, `ToolError`, `ProviderError`, + 5 sentinel errors (counted as one group). Room to spare.
- **No sealed interfaces.** All interfaces are user-implementable.
- **No hidden globals.** No `Init()`, no hub object.
- **`context.Context` first parameter everywhere.**
- **All errors wrap with `%w`.** `errors.Is`, `errors.As` are the only discriminators users need.
- **Zero-value agents are usable** after supplying Provider, Log, and Config — Budget and Tracer are optional.

---

## 10. What's deliberately missing (non-goals — see PRD §9)

- No prompt templating (write Go strings)
- No vector stores (write a Tool)
- No multi-agent coordination
- No chain/flow DSL
- No HTTP server wrapper
- No built-in retry policies (tools/providers wrap themselves)
- No hub/registry global
