# Starling — Design Decisions

> Resolutions to PRD §15 open questions + new decisions surfaced by research.
> ADR-style: each decision has status, context, decision, consequences.
> Last updated: apr 2026

---

## D-001 — Event log is a user-facing concept

**Status:** Accepted
**Context:** PRD §15 asks whether the event log is a library detail or user-facing. Research confirms: it's the entire differentiator. ADK exposes events via `session.Service`; Eino hides checkpoints behind a Resume API.
**Decision:** Event log is the primary user-facing concept. The `EventLog` interface is public. Users read, stream, and replay logs directly. Docs lead with the log, not the agent.
**Consequences:** Events must have a stable wire format (versioned schema from day one). Event types are part of the public API and change only via semver. Users can build their own tooling (UIs, auditors) against the log.

---

## D-002 — Agents are stateless; state lives in the log

**Status:** Accepted
**Context:** Two options: stateful `*Agent` struct that holds current run state, or stateless agent that derives state from the log each call.
**Decision:** Stateless. `Agent` holds configuration (Provider, Tools, Log, Budget); all per-run state lives in the event log. `Run()` creates a new log; `Resume(runID)` reads an existing log.
**Consequences:** Agents are trivially concurrent-safe. Two processes can resume the same run given a shared log backend (care needed for write conflicts — log backend handles). The log read on Resume must be cheap or cached; we'll add log-level snapshots in M3 if needed.

---

## D-003 — Adopt Temporal's determinism model fully for the agent loop

**Status:** Accepted
**Context:** The agent loop calls LLMs, tools, time, and random. Without determinism, replay is impossible. PRD §15 asks how much of Temporal's model to adopt.
**Decision:** Fully. Forbidden in agent-loop code: `time.Now`, `math/rand`, goroutines, direct I/O. Replacements via `step` package: `step.Now(ctx)`, `step.Random(ctx)`, `step.SideEffect(ctx, fn)`. All LLM and tool calls go through `step.LLMCall(ctx, req)` and `step.CallTool(ctx, name, args)`. (Package named `step` to avoid stdlib `runtime` collision.)
**Consequences:** Steeper learning curve for users writing custom agent loops (though most users write tools, not loops). Worth it: replay becomes a trivial property, not a wishful claim. Tools themselves are non-deterministic (like Activities) — only the loop is constrained.

---

## D-004 — Default replay = state-faithful; opt-in re-execution

**Status:** Accepted
**Context:** Research flagged this: "replay produces identical output" is literally false for any temp>0 model. Two coherent definitions exist. Must pick loudly.
**Decision:** Default mode is **state-faithful replay** — replay reads recorded LLM and tool outputs from the log and never re-invokes providers. Opt-in **re-execution replay** mode uses a response cache keyed by `sha256(canonical(prompt) || canonical(params) || model_id)`; miss → call provider, hit → serve recorded. Both modes verify the agent loop's command sequence matches the log.
**Consequences:** Debugging, auditing, CI tests use state-faithful — fast and deterministic. "What-if" analysis (edit prompt, see new outputs) uses re-execution — requires budget, provider API access. Every LLM event records `model_id, params, seed, provider_request_id, raw_response_hash` so drift is detectable.

---

## D-005 — Commands ≠ Events

**Status:** Accepted
**Context:** Temporal's core insight. Agent loop emits commands ("schedule this tool call"); runtime reifies as events ("tool call scheduled").
**Decision:** Internal split: agent loop returns `Command` values; runtime appends `Event` values to the log. Replay diffs next command against next event; mismatch = non-determinism error. Users see events, not commands.
**Consequences:** Clean replay semantics. Command types are internal; event types are public. Command handlers are the only place side effects can originate — a chokepoint for enforcement (budgets, cancellation).

---

## D-006 — One completion event per LLM turn; tokens stream off-log

**Status:** Accepted
**Context:** Should streaming tokens be recorded as events? Research says no — too much volume, couples log to provider chunking, hash chain thrashes.
**Decision:** One `AssistantMessageCompleted` event per turn with full text + `raw_response_hash`. For live UI streaming, publish tokens on a side pubsub channel keyed by `turn_id`. The pubsub channel is not part of the log and is not replayed.
**Consequences:** Log volume stays bounded by turns, not tokens. Live UI callers get token-level updates via a separate API (`Stream()`). Replay produces the completed message directly — no replaying chunks.

---

## D-007 — Tool calls are always two events (Scheduled + Completed|Failed)

**Status:** Accepted
**Context:** Research: this is non-negotiable for (a) resume-after-crash knowing call was in-flight, (b) retry audit, (c) idempotency keys on Scheduled.
**Decision:** Every tool invocation emits `ToolCallScheduled` then either `ToolCallCompleted` or `ToolCallFailed`. Retries append new `ToolCallCompleted|Failed` events sharing `call_id` with incrementing `attempt`. Originals are never mutated.
**Consequences:** Users reconstructing per-call history iterate events sharing `call_id`. Budget enforcement can trigger between Scheduled and Completed (log `BudgetExceeded` event with `call_id` reference).

> **Update (2026-04):** `eventlog.Validate` now enforces this pairing
> semantically: every `ToolCallScheduled` must have exactly one
> `ToolCallCompleted` or `ToolCallFailed` with the same `(CallID,
> Attempt)`. A `RunResumed` seam (kind 15) clears pending pairings —
> orphaned schedules from a crashed run are not required to close after
> the seam, since `Resume` reissues them under fresh `CallID`s.

---

## D-008 — Per-run hash chain; optional Merkle root

**Status:** Accepted
**Context:** Audit/compliance story requires tamper-evidence. Options: global hash chain, per-run hash chain, Merkle tree.
**Decision:** Each event carries `prev_hash` = BLAKE3 over canonical CBOR of the previous event **in the same run**. Per-run chain — simpler sharding, cheaper concurrent writes across runs. On `RunCompleted`, compute a Merkle root over all events and include it in the terminal event. Publishing that root commits the entire run.
**Consequences:** Tamper of any event detectable locally. External audit can be bootstrapped by publishing Merkle roots (e.g., to a transparency log). Per-run (not global) means you can't prove ordering across runs — acceptable for v0.

---

## D-009 — Canonical encoding is CBOR

**Status:** Accepted
**Context:** Hash chain requires canonical bytes. JSON is not deterministic (key order, number representation). Options: canonical JSON (RFC 8785), CBOR (RFC 8949 §4.2), Protobuf.
**Decision:** CBOR with deterministic encoding (RFC 8949 §4.2). Easier than canonical JSON, faster than Protobuf schema management, widely supported in Go (`fxamacker/cbor/v2`).
**Consequences:** Events on disk are CBOR. Public API exposes Go structs — CBOR is internal. An explicit `Event.Marshal()` / `UnmarshalEvent(bytes)` pair in the public API. JSON dump for debugging via a separate helper.

---

## D-010 — Event log default: in-memory (M1), SQLite + Postgres (M2), pluggable everywhere

> **Update (2026-04):** Both backends now ship behind a forward-only
> migration system (`eventlog.Migrate`, `eventlog.SchemaVersion`,
> `eventlog.Preflight`). `Agent.Run` / `Resume` and the inspector
> refuse to operate against a stale or too-new schema. SQLite v2
> renames the legacy `events` table to `eventlog_events` so dump/restore
> is symmetric across backends. See `docs/DEPLOYMENT.md` § Schema
> migrations.

**Status:** Accepted (supersedes PRD §15's walrus direction — walrus is not prod-grade and building a bespoke WAL is not the value prop)
**Context:** M1 needs something fast. M2 needs a durable, crash-safe backend. Target users split into two groups: (a) CLI tools / single-server deploys / demos — want zero ops, and (b) backend services — already have Postgres.
**Decision:**
- M1: in-memory `EventLog` (map + mutex) — enough to prove architecture
- M2: **SQLite** as the default durable backend (`eventlog.NewSQLite(path)`) via `modernc.org/sqlite` (pure Go, no CGO). Schema: `events(run_id TEXT, seq INTEGER, kind INTEGER, prev_hash BLOB, ts INTEGER, payload BLOB, PRIMARY KEY(run_id, seq))`.
- M2: **Postgres** as a first-class pluggable backend (`eventlog.NewPostgres(db)`) — same schema, indexed for the same queries. Shipped in the same release.
- Users can implement `EventLog` themselves (Pebble, BadgerDB, S3, etc.) — it's a narrow 4-method interface.
**Consequences:** Two backends means a shared conformance test suite both must pass — worth it for the onboarding-vs-ops-infra split. SQLite write throughput is lower than a raw append-only WAL, but LLM agent runs produce hundreds of events (not millions) — not a bottleneck. The append-only-file reference implementation is also worth shipping as a documented minimal example (~200 lines) for users who want to understand the invariants.

---

## D-011 — Tool interface: name + description + schema + typed execute

**Status:** Accepted
**Context:** Research surveyed Eino (5 interfaces), langchaingo (single-string input), ADK (no Call method), Genkit (generics + hub). Goal: minimal, typed, no hub, no generics gymnastics.
**Decision:**
```go
type Tool interface {
    Name() string
    Description() string
    Schema() json.RawMessage            // JSON Schema of args
    Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}
```
A generic helper `tool.Typed[In, Out](name, desc, fn)` wraps this for users who prefer typed args — it generates the schema via reflection and handles marshal/unmarshal. The core interface stays JSON-raw for provider compat.
**Consequences:** No `*Genkit`-style hub. No sealed interfaces. Advanced users can implement `Tool` directly; most use `tool.Typed`. Parallel tool calls are orthogonal — the runtime decides parallelism, not the Tool.

---

## D-012 — MCP: optional in M1, native in M4

**Status:** Superseded — see *Updates* below.
**Context:** MCP is the emerging tool standard. Worth native support, but not critical for the POC.
**Decision:** M1–M3 ship without MCP. M4 adds a native MCP adapter: an MCP server's exposed tools become `Tool` instances. Users can pre-M4 shim MCP manually if they want.
**Consequences:** M1 doesn't block on MCP spec churn. M4 adds credibility for the launch ("speaks the protocol").

> **Update (2026-04):** MCP integration is descoped to an *adapter* —
> map MCP server tools onto `tool.Tool` — rather than a "native"
> integration. The core agent runtime stays MCP-agnostic; `tool/mcp`
> translates MCP server tools into ordinary Starling tools. The first
> shipped version supports stdio command servers, streamable HTTP endpoints,
> include/exclude filters, per-call timeout, and output size limits. MCP
> resources, prompts, sampling, elicitation, and logging remain out of scope.

---

## D-013 — Streaming cost enforcement: Anthropic-native, others via tiktoken

**Status:** Accepted
**Context:** Research: Anthropic gives cumulative output tokens mid-stream; OpenAI/Gemini don't. Must handle both.
**Decision:** `Budget` struct with `MaxInputTokens`, `MaxOutputTokens`, `MaxUSD`, `MaxWallClock`. Runtime wraps LLM calls with a watchdog goroutine:
- Anthropic: read `message_delta.usage.output_tokens` (take latest — it's cumulative)
- OpenAI/Gemini: run tiktoken on accumulated text every N chunks; byte-length/4 as fast estimate
When estimated cost ≥ budget: watchdog calls `cancel()` on the request context. Runtime appends `BudgetExceeded` event + `RunFailed` event. Partial output preserved in `BudgetExceeded.payload.partial_text`.
**Consequences:** Exact budget for Anthropic; best-effort for others. Docs make this explicit. Input tokens committed at `message_start` — charged up front against budget, not streamed.

---

## D-014 — Provider API version recorded in events (versioning deferred to M5)

> **Update (2026-04):** Four adapters ship — OpenAI, Anthropic, Gemini,
> OpenRouter. `RawResponseHash` may be enforced strictly via
> `Config.RequireRawResponseHash` for audit-critical deployments. A
> shared provider conformance suite is on the roadmap (W4) and will
> become the canonical contract test for any new adapter.

**Status:** Accepted (matches PRD §15)
**Context:** Model providers deprecate and version. Can't solve it v0, but must not paint ourselves into a corner.
**Decision:** Every `TurnStarted` and `AssistantMessageCompleted` records `provider_id`, `model_id`, `api_version`, `params_snapshot`. No versioning/patching machinery yet. In M5 we'll add `agent.GetVersion(...)` à la Temporal once we've seen real migrations.
**Consequences:** Users can analyze logs by model version. When a model is deprecated, logs retain enough to prove behavior against the deprecated version.

---

## D-015 — Reasoning events optional and separately accessible

**Status:** Accepted
**Context:** Research: reasoning content is sensitive; some models expose it, many don't. Don't force users to either fabricate or silently drop.
**Decision:** `ReasoningEmitted` event type is optional — emitted only when the provider actually returned reasoning content (e.g., Anthropic extended thinking). Event carries content + is flagged `sensitive: true` so log backends can encrypt/redact independently of other events.
**Consequences:** Replay works with or without reasoning events. Storage backends can redact reasoning without breaking the audit chain (the `raw_response_hash` on `AssistantMessageCompleted` is what the chain covers).

---

## D-016 — Parallel tool calls: sibling events under a turn

**Status:** Accepted
**Context:** Modern agents emit multiple tool calls per LLM turn. Log must reflect the fan-out.
**Decision:** All tool calls scheduled in a single turn share a `turn_id`. They run in parallel via `errgroup.WithContext`. Their Scheduled events are siblings under that turn; their Completed/Failed events can arrive in any order (log records arrival order, not dispatch order).
**Consequences:** Replay does not re-execute tools; it reads recorded results. Replay order for parallel results = log order. No need to force deterministic parallel execution ordering.

---

## D-017 — Observability: OTel spans wrap every event, not replace events

**Status:** Accepted
**Context:** OTel is the Go observability standard. Users want traces in Jaeger/Tempo/Honeycomb.
**Decision:** Every event emission also emits an OTel span. Events → durable audit; spans → traces/metrics. Default: no-op OTel; users provide a `TracerProvider` via options. Log is independent of OTel — OTel can be off and nothing breaks.
**Consequences:** Two systems of record, but clearly separated roles. Users who want cost dashboards use OTel metrics derived from events (`BudgetExceeded`, token counts). Users who want audit use the log.

---

## D-018 — Package layout: single module, multiple packages

**Status:** Accepted
**Context:** Go convention favors small packages with clear boundaries. PRD wants < 10 exported types in core.
**Decision:** Module: `github.com/jerkeyray/starling`. Root package exposes `Agent`, `RunResult`, `StepEvent`, `Config`, `Budget`. Subpackages: `event`, `log`, `provider`, `provider/anthropic`, `tool`, `runtime`, `budget`, `replay`, `internal/cborenc`, `examples/code-review`. See ARCHITECTURE.md for detail.
**Consequences:** Small root keeps the 10-type budget. Subpackages can evolve independently. Users import the packages they need.

---

## D-019 — Error types: typed, errors.Is-friendly

**Status:** Accepted
**Context:** Go convention. Users need to distinguish budget-exceeded from tool-failed from provider-errored.
**Decision:** Sentinel errors + typed error structs:
```go
var ErrBudgetExceeded = errors.New("starling: budget exceeded")
var ErrNonDeterminism = errors.New("starling: non-determinism detected during replay")
type ToolError struct { Name string; Err error }
type ProviderError struct { Provider string; Code int; Err error }
```
All errors wrap with `%w`. `errors.Is` and `errors.As` work everywhere.
**Consequences:** Clear contract for users. Observability systems can bucket errors by type. Adding new error categories is semver-trivial (new sentinel).

---

## D-020 — No built-in retry policy; tools retry themselves

**Status:** Accepted
**Context:** Temporal has complex retry policies. For v0, keep retries outside the runtime.
**Decision:** Runtime does not retry tool calls or LLM calls. Tools that want retries wrap themselves (`tool.WithRetry(base, policy)`). Providers that want retries wrap themselves. Log records each attempt as its own Scheduled → Completed/Failed pair.
**Consequences:** Smaller API surface. Users in control. If we need retry policies in M3+, we add them as orthogonal wrappers. Log semantics unchanged — more events, no new event types.

---

## D-021 — OpenAI + OpenAI-compatible providers first; Anthropic in M3

**Status:** Accepted (supersedes PRD §8 M1 scope which named Anthropic as the first provider)
**Context:** The OpenAI Chat Completions API is mirrored by a large provider family — Azure OpenAI, Groq, Together, OpenRouter, Anyscale, Fireworks, DeepSeek, Mistral (compat mode), Ollama, vLLM, LM Studio, llama.cpp server. Shipping an OpenAI adapter with a `WithBaseURL` option unlocks all of them via one package.
**Decision:**
- M1 ships `provider/openai` as the priority adapter. Supports `WithAPIKey`, `WithBaseURL`, `WithHTTPClient`, `WithOrganization`.
- `provider/anthropic` scaffold lives in the repo but is filled in at M3 alongside Gemini.
- OpenAI-compatible providers are tested against at least one non-OpenAI endpoint during M1 (Groq or a local Ollama) to keep the abstraction honest.
**Consequences:**
- **Streaming budget enforcement is best-effort on this family.** OpenAI only includes usage in the final stream chunk (via `stream_options: {include_usage: true}`). If we cancel mid-stream on budget, we forfeit the authoritative count. Mid-stream enforcement uses local tiktoken estimates on accumulated text — accurate within ~5–10%, exact only on stream completion. Documented explicitly.
- **`tiktoken-go` becomes an M1 dependency** (not M3 as originally scoped). Used for pre-call input counting and mid-stream output estimation.
- **Reasoning events are degraded for OpenAI o-series models.** Reasoning content is hidden by OpenAI; we can only record `reasoning_tokens` counts from `usage.completion_tokens_details.reasoning_tokens`. `ReasoningEmitted` event gets a content-optional variant.
- **Provider abstraction stays honest.** The OpenAI adapter cannot be a thin wrapper — it normalizes Chat Completions SSE deltas into our `StreamChunk` shape, which is the same shape Anthropic will produce later. Diverging only here would be a red flag.

---

## Open decisions deferred to implementation

- Exact field layout of each event type → **see EVENTS.md**
- Exact encoding of `prev_hash` as CBOR binary or hex → TBD
- Default CBOR tag numbers (if any) → TBD
- Whether `Stream()` method is on `Agent` or a separate `Streamer` → lean toward method, decide at M3
- Signal handling (how users inject messages mid-run) → deferred to M3
