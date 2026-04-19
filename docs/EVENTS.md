# Starling — Event Schema

> Exact schema for every event type. Wire format is canonical CBOR (RFC 8949 §4.2).
> Schema version 1. Changes before v1.0 may be breaking; after v1.0, only additive.
> Last updated: apr 2026

---

## 1. Envelope

Every event on the log carries the same envelope:

```go
type Event struct {
    RunID     string    `cbor:"run_id"`        // ULID, per-run identifier
    Seq       uint64    `cbor:"seq"`           // monotonic, per-run, starts at 1
    PrevHash  []byte    `cbor:"prev_hash"`     // BLAKE3 of canonical CBOR of prev event (empty for Seq=1)
    Timestamp int64     `cbor:"ts"`            // unix nanoseconds, from step.Now (not wall clock)
    Kind      Kind      `cbor:"kind"`          // discriminator
    Payload   cbor.RawMessage `cbor:"payload"` // kind-specific struct, CBOR-encoded
}
```

**Hash chain**: `ev.PrevHash = BLAKE3(CanonicalCBOR(prev_event_incl_its_prev_hash))`. The first event has empty `PrevHash`.

**Canonical CBOR rules** (RFC 8949 §4.2):
- Integer encoding: shortest form
- Map keys: sorted lexicographically by byte value
- No indefinite-length items
- Floats: shortest form that round-trips

---

## 2. Event kinds

Closed set — adding a kind is a schema-version bump.

```go
type Kind uint8

const (
    KindRunStarted            Kind = 1
    KindUserMessageAppended   Kind = 2
    KindTurnStarted           Kind = 3
    KindReasoningEmitted      Kind = 4
    KindAssistantMessageCompleted Kind = 5
    KindToolCallScheduled     Kind = 6
    KindToolCallCompleted     Kind = 7
    KindToolCallFailed        Kind = 8
    KindSideEffectRecorded    Kind = 9
    KindBudgetExceeded        Kind = 10
    KindContextTruncated      Kind = 11
    KindRunCompleted          Kind = 12
    KindRunFailed             Kind = 13
    KindRunCancelled          Kind = 14
    KindTurnFailed            Kind = 15   // reserved for M3 streaming failures
)
```

---

## 3. Payloads

### 3.1 RunStarted (Kind=1)

First event in every run. Pins provider, model, params, and tool registry.

```go
type RunStarted struct {
    SchemaVersion    uint32          `cbor:"schema_version"`  // =1
    Goal             string          `cbor:"goal"`             // user-provided input
    ProviderID       string          `cbor:"provider_id"`      // e.g. "anthropic"
    ModelID          string          `cbor:"model_id"`         // e.g. "claude-opus-4-7"
    APIVersion       string          `cbor:"api_version"`      // provider-specific
    ParamsHash       []byte          `cbor:"params_hash"`      // BLAKE3 of canonical-CBOR(params)
    Params           cbor.RawMessage `cbor:"params"`           // provider call params snapshot
    SystemPromptHash []byte          `cbor:"system_prompt_hash"`
    SystemPrompt     string          `cbor:"system_prompt"`
    ToolRegistryHash []byte          `cbor:"tool_registry_hash"` // BLAKE3 over sorted tool {name, schema_hash}
    ToolSchemas      []ToolSchemaRef `cbor:"tool_schemas"`
    BudgetLimits     *BudgetLimits   `cbor:"budget,omitempty"`
}

type ToolSchemaRef struct {
    Name       string `cbor:"name"`
    SchemaHash []byte `cbor:"schema_hash"`
}

type BudgetLimits struct {
    MaxInputTokens  int64   `cbor:"max_input_tokens,omitempty"`
    MaxOutputTokens int64   `cbor:"max_output_tokens,omitempty"`
    MaxUSD          float64 `cbor:"max_usd,omitempty"`
    MaxWallClockMs  int64   `cbor:"max_wall_clock_ms,omitempty"`
}
```

### 3.2 UserMessageAppended (Kind=2)

Emitted when `Resume` injects a new user message into an in-progress run.

```go
type UserMessageAppended struct {
    Content string `cbor:"content"`
}
```

### 3.3 TurnStarted (Kind=3)

A new LLM call is about to be issued.

```go
type TurnStarted struct {
    TurnID       string `cbor:"turn_id"`        // ULID
    PromptHash   []byte `cbor:"prompt_hash"`    // BLAKE3 of canonical CBOR of prompt messages
    InputTokens  int64  `cbor:"input_tokens"`   // committed up-front (from message_start for Anthropic)
}
```

### 3.4 ReasoningEmitted (Kind=4) — optional

Emitted only when the provider returned explicit reasoning (e.g., Anthropic extended thinking). Content flagged sensitive.

```go
type ReasoningEmitted struct {
    TurnID    string `cbor:"turn_id"`
    Content   string `cbor:"content"`
    Sensitive bool   `cbor:"sensitive"`  // always true; present for schema symmetry
}
```

### 3.5 AssistantMessageCompleted (Kind=5)

Fires when the LLM stream ends successfully. Carries the full turn output.

```go
type AssistantMessageCompleted struct {
    TurnID            string            `cbor:"turn_id"`
    Text              string            `cbor:"text"`
    ToolUses          []PlannedToolUse  `cbor:"tool_uses,omitempty"`
    StopReason        string            `cbor:"stop_reason"`        // provider-native
    InputTokens       int64             `cbor:"input_tokens"`
    OutputTokens      int64             `cbor:"output_tokens"`
    CacheReadTokens   int64             `cbor:"cache_read_tokens,omitempty"`
    CacheCreateTokens int64             `cbor:"cache_create_tokens,omitempty"`
    CostUSD           float64           `cbor:"cost_usd"`           // computed locally
    RawResponseHash   []byte            `cbor:"raw_response_hash"`  // BLAKE3 of canonicalized provider response
    ProviderRequestID string            `cbor:"provider_request_id,omitempty"`
}

type PlannedToolUse struct {
    CallID   string          `cbor:"call_id"`   // ULID, referenced by ToolCallScheduled
    ToolName string          `cbor:"tool"`
    Args     cbor.RawMessage `cbor:"args"`
}
```

### 3.6 ToolCallScheduled (Kind=6)

Runtime is about to dispatch a tool call.

```go
type ToolCallScheduled struct {
    CallID   string          `cbor:"call_id"`
    TurnID   string          `cbor:"turn_id"`
    ToolName string          `cbor:"tool"`
    Args     cbor.RawMessage `cbor:"args"`
    Attempt  uint32          `cbor:"attempt"`   // 1 on first try; retries increment
    IdempKey string          `cbor:"idemp_key,omitempty"`  // user-set via tool options
}
```

### 3.7 ToolCallCompleted (Kind=7)

```go
type ToolCallCompleted struct {
    CallID     string          `cbor:"call_id"`
    Result     cbor.RawMessage `cbor:"result"`
    DurationMs int64           `cbor:"duration_ms"`
    Attempt    uint32          `cbor:"attempt"`
}
```

### 3.8 ToolCallFailed (Kind=8)

```go
type ToolCallFailed struct {
    CallID     string `cbor:"call_id"`
    Error      string `cbor:"error"`
    ErrorType  string `cbor:"error_type"`       // "timeout" | "panic" | "tool" | "cancelled"
    DurationMs int64  `cbor:"duration_ms"`
    Attempt    uint32 `cbor:"attempt"`
}
```

### 3.9 SideEffectRecorded (Kind=9)

For `step.Now`, `step.Random`, `step.SideEffect(fn)` — any non-deterministic value consumed by the agent loop.

```go
type SideEffectRecorded struct {
    Name  string          `cbor:"name"`    // "now" | "rand" | user-defined
    Value cbor.RawMessage `cbor:"value"`   // CBOR-encoded return value
}
```

### 3.10 BudgetExceeded (Kind=10)

Runtime cancelled an LLM call mid-stream (or pre-call) due to budget.

```go
type BudgetExceeded struct {
    Limit         string  `cbor:"limit"`   // "input_tokens" | "output_tokens" | "usd" | "wall_clock"
    Cap           float64 `cbor:"cap"`
    Actual        float64 `cbor:"actual"`
    Where         string  `cbor:"where"`   // "pre_call" | "mid_stream" | "post_call"
    TurnID        string  `cbor:"turn_id,omitempty"`
    CallID        string  `cbor:"call_id,omitempty"`
    PartialText   string  `cbor:"partial_text,omitempty"`
    PartialTokens int64   `cbor:"partial_tokens,omitempty"`
}
```

### 3.11 ContextTruncated (Kind=11)

Context window management trimmed prior messages.

```go
type ContextTruncated struct {
    Strategy       string `cbor:"strategy"`         // "drop_oldest" | "summary" | user-defined
    TokensBefore   int64  `cbor:"tokens_before"`
    TokensAfter    int64  `cbor:"tokens_after"`
    MessagesBefore uint32 `cbor:"messages_before"`
    MessagesAfter  uint32 `cbor:"messages_after"`
}
```

### 3.12 RunCompleted (Kind=12)

Terminal. Includes Merkle root over all prior events.

```go
type RunCompleted struct {
    FinalText      string  `cbor:"final_text"`
    TurnCount      uint32  `cbor:"turn_count"`
    ToolCallCount  uint32  `cbor:"tool_call_count"`
    TotalCostUSD   float64 `cbor:"total_cost_usd"`
    TotalInputTokens  int64 `cbor:"total_input_tokens"`
    TotalOutputTokens int64 `cbor:"total_output_tokens"`
    DurationMs     int64   `cbor:"duration_ms"`
    MerkleRoot     []byte  `cbor:"merkle_root"`     // BLAKE3 Merkle over all prior event hashes
}
```

### 3.13 RunFailed (Kind=13)

```go
type RunFailed struct {
    Error       string  `cbor:"error"`
    ErrorType   string  `cbor:"error_type"`   // "budget" | "nondeterminism" | "provider" | "tool" | "internal"
    MerkleRoot  []byte  `cbor:"merkle_root"`
    DurationMs  int64   `cbor:"duration_ms"`
}
```

### 3.14 RunCancelled (Kind=14)

```go
type RunCancelled struct {
    Reason     string `cbor:"reason"`     // "user" | "context_cancelled" | "deadline"
    MerkleRoot []byte `cbor:"merkle_root"`
    DurationMs int64  `cbor:"duration_ms"`
}
```

### 3.15 TurnFailed (Kind=15) — M3

Reserved. Fires when a streaming LLM call fails mid-turn but run continues (retry). Not emitted in M1.

---

## 4. Invariants

1. **Seq is monotonic per run.** Starts at 1. No gaps, no duplicates.
2. **First event is always `RunStarted`.** `PrevHash = nil`.
3. **Last event is always one of `RunCompleted`, `RunFailed`, `RunCancelled`.** These carry the Merkle root.
4. **`TurnStarted` always followed by `AssistantMessageCompleted`, `BudgetExceeded`, or `TurnFailed` (M3+)** — same `turn_id`. No other terminal states for a turn.
5. **`ToolCallScheduled` always followed by `ToolCallCompleted` or `ToolCallFailed`** — same `call_id` and `attempt`.
6. **Retries share `call_id`, increment `attempt`.** Original events never mutated.
7. **Reasoning events are optional.** A `TurnStarted` may or may not produce a `ReasoningEmitted` before `AssistantMessageCompleted`.
8. **Hash chain integrity.** Any event's `PrevHash` equals BLAKE3 of canonical CBOR of the full previous event.
9. **Merkle root in terminal event equals** BLAKE3-merkle over hashes of all prior events in this run.

---

## 5. Validation rules

A well-formed log satisfies all invariants above. The `replay.Validate(runID)` function checks:

1. Seq monotonicity
2. First event is `RunStarted`, schema version supported
3. Terminal event exists and is exactly one of the three terminal kinds
4. Hash chain unbroken
5. Turn pairing: every `TurnStarted` has a matching terminal turn event
6. Call pairing: every `ToolCallScheduled` has a matching `ToolCallCompleted`/`ToolCallFailed` with same `call_id`, `attempt`
7. Merkle root matches recomputed root

Invalid log → `ErrLogCorrupt` with a field locating the violation.

---

## 6. Worked example

A run with one LLM turn that makes two parallel tool calls:

```
seq=1  RunStarted         prev=nil
seq=2  TurnStarted        prev=H(seq1)   turn=T1
seq=3  AssistantMessageCompleted prev=H(seq2) turn=T1, two PlannedToolUses C1, C2
seq=4  ToolCallScheduled  prev=H(seq3)   call=C1, turn=T1, attempt=1
seq=5  ToolCallScheduled  prev=H(seq4)   call=C2, turn=T1, attempt=1
seq=6  ToolCallCompleted  prev=H(seq5)   call=C2, attempt=1   (finished first)
seq=7  ToolCallCompleted  prev=H(seq6)   call=C1, attempt=1
seq=8  TurnStarted        prev=H(seq7)   turn=T2
seq=9  AssistantMessageCompleted prev=H(seq8) turn=T2, no tool uses (done)
seq=10 RunCompleted       prev=H(seq9)   merkle_root=M(seq1..seq9)
```

Note parallel tool calls finish in arrival order (C2 first, then C1). Log preserves this — replay reproduces it deterministically because results are read from the log, not re-executed.

---

## 7. Size expectations

Rough order-of-magnitude per event:

- `RunStarted`: 1–10 KB (depends on system prompt + tool schemas)
- `TurnStarted`: <1 KB
- `AssistantMessageCompleted`: 2–50 KB (depends on model output)
- `ToolCallScheduled`/`Completed`: 1–20 KB each (tool I/O)
- Terminal events: <1 KB

A typical 5-turn run with 10 tool calls: ~100 KB–500 KB. Not a concern for in-memory in M1; trivially within SQLite/Postgres write throughput in M2+.

---

## 8. Schema evolution

Before v1.0: any change permitted; schema version bump when breaking.
After v1.0:
- Additive changes (new optional fields, new event kinds) → minor version
- Breaking changes (removed fields, changed meaning) → major version
- Replayer refuses to replay a log whose schema version is newer than its own major
- Old logs remain replayable forever by pinning `starling` version or via migration tools shipped at major bumps
