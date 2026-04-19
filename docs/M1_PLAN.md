# Starling — M1 Implementation Plan

> M1 exit criteria (PRD §8): An agent runs, finishes, and you can inspect the complete event log afterwards.
> Target: weeks 5–7 (3 weeks).
> Scope is deliberately narrow — no replay yet, no streaming cost enforcement, no multi-provider.
> Last updated: apr 2026

---

## M1 scope

**In scope:**
- Event types + canonical CBOR encoding + BLAKE3 hash chain
- In-memory `EventLog`
- OpenAI provider + OpenAI-compatible base URL support (streaming, normalized to `StreamChunk`)
- Sequential tool execution (parallel deferred to M3)
- ReAct-ish agent loop that writes every decision to the log
- Two built-in tools: `Fetch`, `ReadFile`
- `Agent.Run()` end-to-end
- `tool.Typed[In, Out]` generic helper

**Out of scope (deferred):**
- Replay (`Agent.Replay`) → M2
- SQLite + Postgres durable backends → M2
- Determinism enforcement with `step.Now/Random/SideEffect` → partial in M1 (structure in place, not wired); real enforcement M2
- OpenAI / Gemini providers → M3
- Streaming cost budgets → M3 (budget struct exists in M1, pre-call check only)
- Parallel tool calls → M3
- OpenTelemetry spans → M3
- MCP → M4

---

## Task breakdown

Each task has: dependencies, deliverable, exit test. Order matters — tasks assume prior tasks are complete.

### T1 — module scaffold (0.5 day)

**Depends on:** nothing

**Deliverable:**
- `go mod init github.com/jerkeyray/starling`
- Directory structure per ARCHITECTURE.md §2
- `.github/workflows/ci.yml` running `go vet ./...` and `go test ./...`
- README placeholder with "status: pre-alpha"
- LICENSE (MIT or Apache-2.0; pick and move on)

**Exit test:** `go build ./...` succeeds with empty files. CI runs green on empty repo.

---

### T2 — canonical CBOR + BLAKE3 helpers (1 day)

**Depends on:** T1

**Deliverable:**
- `internal/cborenc/cborenc.go` — deterministic CBOR encode/decode wrappers around `fxamacker/cbor/v2`
- `event/hash.go` — `HashEvent(prev, ev)` returning BLAKE3(canonical_cbor(ev_with_prev))
- Property test: encoding the same struct twice gives byte-identical output
- Test: hash chain over 100 random events verifies correctly

**Exit test:** `go test ./internal/cborenc ./event -run Hash -count 10` passes.

---

### T3 — event type definitions (1 day)

**Depends on:** T2

**Deliverable:**
- `event/event.go` — `Event` envelope, `Kind` enum, all 14 kinds defined per EVENTS.md §2
- `event/types.go` — payload structs for all kinds
- `event/encoding.go` — `Marshal(Event) ([]byte, error)`, `Unmarshal([]byte) (Event, error)`, `Event.Payload` typed accessors (`AsRunStarted`, etc.)
- Round-trip test: every payload type marshals and unmarshals cleanly
- Golden test: a hand-constructed event encodes to a fixed byte sequence (freezes the wire format)

**Exit test:** `go test ./event` passes. Golden test fixtures committed to `event/testdata/`.

---

### T4 — in-memory EventLog (0.5 day)

**Depends on:** T3

**Deliverable:**
- `eventlog/eventlog.go` — `EventLog` interface
- `eventlog/memory.go` — `NewInMemory()` implementation:
  - `map[runID][]event.Event` behind `sync.RWMutex`
  - Enforces Seq monotonicity on Append
  - Auto-computes `PrevHash` from prior event
  - `Stream()` returns a new channel; Append publishes to all open subscribers for that run
- Test: 100 concurrent appends across 10 runs → all reads return correctly ordered chains

**Exit test:** `go test ./log -race` passes.

---

### T5 — Provider interface + OpenAI adapter (3 days)

**Depends on:** T1

Per D-021, OpenAI is the priority provider. The same adapter serves OpenAI-compatible APIs (Groq, Together, OpenRouter, Ollama, vLLM, LM Studio, etc.) via `WithBaseURL`.

**Deliverable:**
- `provider/provider.go` — `Provider`, `Request`, `Message`, `EventStream`, `StreamChunk`, `UsageUpdate`, `ChunkKind` per API.md §4
- `provider/openai/openai.go` — implements `Provider`:
  - Uses `github.com/openai/openai-go`
  - Options: `WithAPIKey`, `WithBaseURL` (unlocks compat providers), `WithHTTPClient`, `WithOrganization`
  - Request sets `stream_options.include_usage = true` so the terminal chunk carries final usage
  - Maps Chat Completions SSE deltas → normalized `StreamChunk`s:
    - First delta with `choices[].delta.content` → `ChunkText` (repeat for each delta)
    - Delta with `choices[].delta.tool_calls[].function.name` → `ChunkToolUseStart`
    - Delta with `choices[].delta.tool_calls[].function.arguments` → `ChunkToolUseDelta`
    - Tool-call `finish_reason=tool_calls` → `ChunkToolUseEnd` for each open call
    - Reasoning deltas (o-series, via `choices[].delta.reasoning`) → `ChunkReasoning` (content present on compatible models, omitted otherwise)
    - Terminal chunk with `usage` → `ChunkUsage` (final, not cumulative) then `ChunkEnd`
  - Tool args buffered across argument deltas, delivered on `ToolUseEnd`
  - Input tokens estimated pre-call via a rune/3 approximation in `step/budget.go` (see T8); the authoritative value arrives in the final `ChunkUsage` from the server
- Integration tests:
  - `OPENAI_API_KEY` gate: `TestLive_OpenAI` — send "Say hello", assert `ChunkText` + `ChunkEnd` arrive
  - Optional `GROQ_API_KEY` gate: `TestLive_Compat_Groq` — same request with `WithBaseURL("https://api.groq.com/openai/v1")` against `llama-3.1-8b-instant`; proves compat path

**Exit test:** `go test ./provider/openai -run TestLive_OpenAI` passes with a real key (gated, skipped in CI). Compat test skipped without Groq key.

---

### T6 — Tool interface + Typed helper + builtins (1 day)

**Depends on:** T1

**Deliverable:**
- `tool/tool.go` — `Tool` interface
- `tool/typed.go` — `Typed[In, Out]` generic wrapper:
  - Uses reflection on zero `In` to generate JSON Schema via `invopop/jsonschema` or hand-rolled
  - Marshal/unmarshal handled automatically
  - Panic in tool body becomes an error return
- `tool/builtin/fetch.go` — `Fetch()` returns a `Tool` that GETs a URL and returns body
- `tool/builtin/readfile.go` — `ReadFile()` reads a path (restricted to a base dir for safety)
- Tests: `Typed` wraps a pure function, executes correctly, schema matches

**Exit test:** `go test ./tool ./tool/builtin` passes.

---

### T7 — runtime context + non-deterministic helpers stubs (0.5 day)

**Depends on:** T3, T4

**Deliverable:**
- `step/context.go` — opaque `Context` (owns log + runID + hash-chain cursor), `NewContext(log, runID)` constructor, `WithContext`/`From` round-trippers. Context is mutex-guarded so concurrent step calls emit a consistent chain.
- `step/step.go` — stubs for `Now`, `Random`, `SideEffect`:
  - **M1 behavior**: `Now` returns `time.Now()` and emits `SideEffectRecorded` event; no replay yet, so "record" and "replay from record" are trivial (record only)
  - `Random` uses crypto/rand + records
  - `SideEffect[T](ctx, name, fn)` runs fn, CBOR-encodes result on success, emits `SideEffectRecorded`. On fn error the error is propagated and no event is emitted.
  - All three panic if called outside a step.Context-carrying ctx — producing a value without an emission makes the run non-replayable.
- Test: calling Now twice emits two SideEffectRecorded events with monotonically increasing timestamps, chained by PrevHash.

**Exit test:** `go test ./step` passes. Note: replay machinery lands in M2; M1 just exercises the "record" side.

---

### T8 — step.LLMCall + step.CallTool (2 days)

**Depends on:** T4, T5, T6, T7

**Deliverable:**
- `step/config.go` — `Config{Log, RunID, Provider, Tools, Budget}`, `BudgetConfig{MaxInputTokens}`, sentinels `ErrBudgetExceeded` / `ErrToolNotFound`. `NewContext` now takes a `Config` (T7's `NewContext(log, runID)` signature broken as part of this task).
- `step/registry.go` — `Registry` owning name→`tool.Tool` lookup; `NewRegistry`, `Get`, alphabetical `Names()` (used by `RunStarted.ToolRegistryHash`).
- `step/budget.go` — `estimateRequestTokens(*provider.Request) int64`: rune count over SystemPrompt + Messages[*].Content + ToolResult.Content + ToolUse.Args + tool schema bytes, divided by 3 (over-counting). **Deliberate deviation from original plan:** tiktoken is skipped. The authoritative post-call count always comes from `ChunkUsage`; the pre-call check is a coarse guardrail that errs on the side of rejecting. Swap in tiktoken later if the approximation proves biting.
- `budget/prices.go` — per-model USD price table + `CostUSD(model, inTok, outTok) (float64, bool)`. Unknown models return `(0, false)` and emit one `sync.Once`-guarded warning per model.
- `step/llm.go` — `LLMCall(ctx, *provider.Request) (*provider.Response, error)`:
  - Pre-call estimate via `estimateRequestTokens`; if `Budget.MaxInputTokens>0` and exceeded, emit `BudgetExceeded{Limit:"input_tokens", Where:"pre_call"}` and return `ErrBudgetExceeded`. **No TurnStarted.**
  - Mint `TurnID` (ULID, crypto/rand entropy, mutex-guarded for concurrent safety).
  - Emit `TurnStarted{TurnID, PromptHash: blake3(canonical-CBOR(req)), InputTokens: estimate}`.
  - Open stream; on open error return unchanged (T9 emits RunFailed).
  - Consume chunks: text into buffer; ReasoningEmitted emitted per chunk (OpenAI M1 emits none); tool-use Start/Delta/End buffered in an ordered slice indexed by CallID; Usage overwrites running total; End records StopReason/RawResponseHash/ProviderReqID. `io.EOF` treated as clean end even without explicit ChunkEnd.
  - Re-encode each tool-use's JSON args into canonical CBOR via `jsonToCanonicalCBOR` (JSON → `any` → CBOR) for `PlannedToolUse.Args`; the provider.Response.ToolUses retains the original JSON bytes for direct hand-off to `CallTool`.
  - Compute cost via `budget.CostUSD(req.Model, in, out)`.
  - Emit `AssistantMessageCompleted` with full payload and return `*provider.Response`.
  - Mid-stream error / ctx cancellation: return error unchanged, **do not** emit AssistantMessageCompleted. T9 handles RunFailed/RunCancelled.
- `step/tools.go` — `CallTool(ctx, ToolCall) (json.RawMessage, error)`:
  - `ToolCall{CallID, TurnID, Name, Args}` — taking a struct preserves the LLM-assigned CallID from `PlannedToolUse.CallID` through `ToolCallScheduled.CallID`. Empty CallID is auto-minted (ULID).
  - Emit `ToolCallScheduled{CallID, TurnID, ToolName, Args (CBOR), Attempt:1}`.
  - Look up tool in Registry; missing → emit `ToolCallFailed{ErrorType:"tool"}`, return `ErrToolNotFound`.
  - Invoke `tl.Execute(ctx, args)` under a recover wrapper that converts panics into `fmt.Errorf("%w: %v", tool.ErrPanicked, r)`.
  - Success → `ToolCallCompleted{Result:CBOR, DurationMs, Attempt:1}`.
  - Classify failures into `{"panic", "cancelled", "tool"}` per EVENTS.md §3.8; `"timeout"` deferred to M3's watchdog.
  - Event log writes use `context.WithoutCancel` so cancellation never drops the audit trail — a Failed-cancelled event always lands.
- Tests: `step/llm_test.go` (fakeProvider over canned chunks; text-only, tool-uses, pre-call budget, stream error, cost lookup, deterministic payload re-encoding) and `step/tools_test.go` (echo success, minted CallID, tool error, panic, cancellation, not-found, missing-context panic).
- **Out of scope for M1:** `CallTools` (parallel errgroup variant) — M2; real tiktoken — deferred; mid-stream budget enforcement — M3.

**Exit test:** `go test ./step -count 5` passes.

---

### T9 — root package: Agent + ReAct loop (1.5 days)

**Depends on:** T8

**Deliverable:**
- `agent.go` — `Agent`, `Config`, `RunResult` types
- `Agent.Run(ctx, goal)`:
  1. Build tool registry (map name → Tool)
  2. Generate `run_id` (ULID)
  3. Emit `RunStarted` with all snapshots (model, params, system prompt, tool schemas)
  4. Append user message to message list, loop:
     a. Call `step.LLMCall` with current messages + tools
     b. If response has no `tool_uses`: emit `RunCompleted`, return
     c. For each `tool_use` (sequentially in M1): call `step.CallTool`, append result as user message
     d. If turn count > `MaxTurns`: emit `RunFailed` (`ErrMaxTurnsExceeded`), return
  5. Compute Merkle root from all events, include in terminal event
- `error.go` — sentinel errors + `ToolError`, `ProviderError` types
- Integration test (no real API): mock Provider + mock Tool, run agent, verify event sequence matches expectation

**Exit test:** `go test ./... -count 3` passes. Event count for a 2-turn-1-tool-call run = exactly 7 events (RunStarted, TurnStarted, AssistantMessageCompleted, ToolCallScheduled, ToolCallCompleted, TurnStarted, AssistantMessageCompleted, RunCompleted = 8; count will be validated).

---

### T10 — end-to-end demo (1 day)

**Depends on:** T9

**Deliverable:**
- `examples/m1_hello/main.go` — minimal runnable example:
  - OpenAI provider with env key (model: `gpt-4o-mini`)
  - `current_time` tool (via `tool.Typed`)
  - Agent.Run("What is the current time?")
  - Print final text + event count + cost
- Manually run against real OpenAI API
- Repeat with `WithBaseURL("https://api.groq.com/openai/v1")` + `GROQ_API_KEY` + `llama-3.1-8b-instant` to prove the compat story
- Confirm event log contains the full sequence including the tool round-trip

**Exit test:** `go run ./examples/m1_hello` succeeds with `OPENAI_API_KEY` set, prints final answer and ≥6 events. Same run with Groq base URL also succeeds.

---

### T11 — budget struct skeleton (0.5 day)

**Depends on:** T9

**Deliverable:**
- `budget/budget.go` — `Budget` struct per API.md §1.4
- Pre-call enforcement wired in `step.LLMCall` (M1: input tokens check only)
- Test: set `MaxInputTokens=1`, call LLM with 100-token prompt, assert `ErrBudgetExceeded` + `BudgetExceeded` event with `where="pre_call"`

**Exit test:** `go test ./budget ./runtime -run Budget` passes.

---

### T12 — log validation + Merkle root (1 day)

**Depends on:** T4

**Deliverable:**
- `event/merkle.go` — BLAKE3 Merkle over event hashes
- `eventlog/validate.go` — `Validate(events []Event) error` checking all invariants from EVENTS.md §4
- Wire into `Agent.Run` terminal events (`RunCompleted`, `RunFailed`, `RunCancelled`): include `merkle_root`
- Test: valid log passes; tampered log (mutate one byte in any event) fails validation

**Exit test:** `go test ./log -run Validate -count 10` passes.

---

### T13 — README + pre-alpha docs (0.5 day)

**Depends on:** T10

**Deliverable:**
- `README.md` — one-page intro:
  - What Starling is (one paragraph)
  - Install (`go get github.com/jerkeyray/starling`)
  - "Hello agent" example from API.md §7, condensed
  - Status: pre-alpha, M1 shipped, M2 next
  - Link to PRD / design docs
- Move `temp_notes/*.md` to `docs/`

**Exit test:** `go doc github.com/jerkeyray/starling` shows coherent package doc; README renders on GitHub with working code block.

---

## Milestone exit criteria

Check all of:

- [ ] `go test ./... -race -count 3` passes
- [ ] `examples/m1_hello` runs end-to-end against real Anthropic API
- [ ] Event log after a run contains all expected kinds in the right order (RunStarted → TurnStarted → AssistantMessageCompleted → [ToolCallScheduled → ToolCallCompleted]* → RunCompleted)
- [ ] `log.Validate` passes for a real run's log
- [ ] Tampering with any event byte causes validation failure
- [ ] Pre-call budget enforcement fires on oversized input
- [ ] Public root package exports ≤ 10 types
- [ ] No `any` in the public root API except where JSON/CBOR genuinely requires it
- [ ] README walks someone to a working agent in under 10 minutes

---

## Rough calendar

| Week | Tasks | Output |
|---|---|---|
| Week 5 | T1, T2, T3, T4 | Event log infra working |
| Week 6 | T5, T6, T7, T8 | Provider + runtime + tools working |
| Week 7 | T9, T10, T11, T12, T13 | Agent runs end-to-end, demo ships, docs up |

Slack: ~2 days buffer across the 3 weeks. Anthropic SDK integration (T5) is the biggest risk — budget 3 days instead of 2.5 if the stream normalization proves nasty.

---

## Open questions to resolve during M1

1. **CBOR library** — `fxamacker/cbor/v2` is canonical and well-maintained. Commit in T2.
2. **BLAKE3 library** — `zeebo/blake3` (pure Go, fastest). Alternative: `lukechampine.com/blake3`. Pick in T2.
3. **JSON Schema generation for `tool.Typed`** — `invopop/jsonschema` vs hand-rolled reflection. Pick in T6. Hand-rolled probably simpler for the narrow types agents use.
4. **ULID library** — `oklog/ulid/v2` (maintained, canonical). Commit in T3.
5. **Anthropic SDK** — `github.com/anthropics/anthropic-sdk-go` (official). Commit in T5.
6. **Cost calc source of truth** — hard-code a per-model price map in `budget/prices.go`; update manually. Pricing-API integration deferred.

---

## What M1 proves

After M1 ships, you can tell the story:

> Starling runs LLM agents. Every decision the agent makes is recorded as an event. You can read the log after the fact to see exactly what happened — every LLM call, every tool invocation, every cost. The log is hash-chained so tampering is detectable, and every run ends with a Merkle root that commits the whole history.

That narrative is enough for the first design post. M2 adds "and you can replay any run and get the same output, bit-for-bit." M3 adds "and you can set a $ budget that we'll enforce mid-stream." That's the full pitch.
