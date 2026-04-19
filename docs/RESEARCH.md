# Starling — Research Notes

> Consolidated findings from deep research on the four pillars of the design: Temporal's event-sourced model, existing Go agent frameworks, event sourcing + ReAct theory, and streaming cost enforcement.
> Last updated: apr 2026

---

## 1. Temporal: the blueprint

### 1.1 Event history model

Every Temporal workflow is an append-only log. The service is the source of truth; workflow code is a pure function over the log. Key event families:

- **Workflow lifecycle**: `WorkflowExecutionStarted/Completed/Failed/TimedOut/ContinuedAsNew/Signaled/CancelRequested`
- **Workflow task loop**: `WorkflowTaskScheduled → Started → Completed | Failed | TimedOut` (the unit of worker progress)
- **Activity lifecycle**: `ActivityTaskScheduled/Started/Completed/Failed/TimedOut/CancelRequested/Canceled`
- **Timers**: `TimerStarted/Fired/Canceled`
- **Side effects**: `MarkerRecorded` (for `SideEffect`, `MutableSideEffect`, `GetVersion`, local activities)

### 1.2 Command vs Event — the load-bearing distinction

Workflow code emits **Commands** ("schedule this activity"). The server reifies them as **Events** ("activity was scheduled"). Replay compares the next command to the next recorded event — a mismatch is non-determinism.

### 1.3 Determinism constraint

Forbidden in workflow code: `time.Now`, `time.Sleep`, `math/rand`, goroutines, native channels, map iteration, file I/O, network calls. Replacements: `workflow.Now`, `workflow.Sleep`, `workflow.NewTimer`, `workflow.Go`, `workflow.Channel`, `workflow.Selector`, `workflow.SideEffect`. Non-deterministic work goes into **Activities**, which run once, are recorded, and the workflow only ever sees the recorded result.

### 1.4 Replay mechanism

1. Worker fetches event history
2. Re-runs workflow function from the top
3. Suspends at the first future that has no corresponding event
4. Emits new commands from that point

Mismatches caught on `ExecuteActivity`, `NewTimer`, `SideEffect`, `SignalExternalWorkflow`, `Sleep`. **The Replayer in CI is the single highest-leverage thing Temporal built.**

### 1.5 Versioning

Two tools: **Worker Versioning** (pin old runs to old workers) and **Patching** (`workflow.GetVersion(ctx, "change-id", 1, 2)` — records a marker, returns recorded version on replay). Never remove a `GetVersion` call — older histories still need the marker.

### 1.6 Known pitfalls

- `MutableSideEffect` behind `GetVersion` desyncs command pointer ([sdk-go#1144](https://github.com/temporalio/sdk-go/issues/1144))
- `asyncio.wait()` on raw coroutines is non-deterministic ([sdk-python#429](https://github.com/temporalio/sdk-python/issues/429))
- Parallel async exception ordering ([sdk-java#902](https://github.com/temporalio/sdk-java/issues/902))
- Local activities insufficiently checked ([sdk-go#983](https://github.com/temporalio/sdk-go/issues/983))

### 1.7 Sources

- [Temporal Workflow overview](https://docs.temporal.io/workflows)
- [Workflow Definition & Deterministic Constraints](https://docs.temporal.io/workflow-definition)
- [Events and Event History](https://docs.temporal.io/workflow-execution/event)
- [Events reference](https://docs.temporal.io/references/events)
- [Go SDK Versioning & Patching](https://docs.temporal.io/develop/go/versioning)
- [Workflow Engine Principles (blog)](https://temporal.io/blog/workflow-engine-principles)
- [Go workflow package API reference](https://pkg.go.dev/go.temporal.io/sdk/workflow)
- [TMPRL1100: Non-determinism rule](https://github.com/temporalio/rules/blob/main/rules/TMPRL1100.md)

---

## 2. Go LLM agent landscape

### 2.1 Eino (ByteDance, cloudwego/eino) — the most mature Go option

- **Tool interface**: `BaseTool` → `InvokableTool` → `EnhancedInvokableTool` × streaming variants = 5 interfaces for one concept (over-abstracted)
- **Runner**: `NewRunner(ctx, RunnerConfig{Agent, EnableStreaming, CheckPointStore})` → `*AsyncIterator[*AgentEvent]`
- **Persistence**: `CheckPointStore` with gob-serialized State, `Runner.Resume` / `ResumeWithParams`. This is their standout feature — but it's snapshot-based resume, not event-sourced replay.
- **Gaps**: No deterministic replay, no cost budgets, config-struct-in-config-struct (`ToolsConfig{ToolsNodeConfig{Tools}}`), graph-compose DSL with string node IDs (LangGraph baggage).

### 2.2 langchaingo (tmc/langchaingo) — what not to do

- **Tool interface**: `Tool { Name, Description, Call(ctx, input string) (string, error) }` — tool input is a single string. No structured args without manual JSON.
- **Agent interface**: `map[string]any` inputs/outputs everywhere; `GetInputKeys()/GetOutputKeys()` is pure Python-LangChain baggage.
- **Gaps**: No persistence/replay/cost. `Chain` abstraction is clearly ported (chains wrap agents wrap LLMs).

### 2.3 Google ADK Go — modern, but heavy on multi-agent

- **Tool interface**: `Tool { Name, Description, IsLongRunning }` — no `Call` method. Execution routes through subtypes (`functiontool`, `agenttool`, `mcptoolset`). `RequestConfirmation` is built into tool context (HITL is first-class).
- **Agent interface**: `Run(InvocationContext) iter.Seq2[*session.Event, error]` — Go 1.23 iterator pattern. Very idiomatic.
- **Sealed interface**: `internal() *agent` — you can't implement `Agent` yourself.
- **Persistence**: `session.Service` (Create/Get/List/Delete/AppendEvent) with InMemory, database, vertexai implementations. Event-sourced-ish — but events are conversational, not a full audit log. No deterministic replay.
- **Gaps**: No cost budgets. Multi-agent machinery bloats the surface (can skip).

### 2.4 Genkit Go (firebase/genkit) — generics + hub

- **Tool definition**: `DefineTool[In, Out](g, name, desc, fn)` — generics, no explicit interface. Registered into a `*Genkit` hub.
- **Gaps**: Hub global-ish, hard to test in isolation. No replay, no cost budgets. Observability via Dev UI only.

### 2.5 Convergent patterns (the Go agent consensus)

1. **Agent as iterator of events** — ADK's `iter.Seq2`, Eino's `AsyncIterator`. Both landed on "agent run is a stream of events."
2. **Tool = Name + Description + typed execute** — all four frameworks.
3. **Runner separate from Agent** — definition vs execution split.
4. **Session service as pluggable interface** — ADK and Eino both converge here.
5. **HITL via interrupt/resume** — Eino `ResumeWithParams`, ADK `RequestConfirmation`.

### 2.6 Starling's real gaps to fill

1. **True event-sourced log** (vs ADK's conversational events or Eino's gob blobs)
2. **Deterministic replay** (no framework does this; it's the testing story nobody solved)
3. **Streaming cost budgets** (zero frameworks enforce mid-stream)
4. **Typed tool args without generics gymnastics** — `Tool[In, Out]` with `Exec(ctx, In) (Out, error)`
5. **No hidden globals, no sealed interfaces**
6. **Skip multi-agent** — it's the bulk of ADK/Eino surface and not our differentiator

---

## 3. Event sourcing + ReAct

### 3.1 Event sourcing fundamentals (Fowler)

- Event log is the system of record; state is derivable cache
- Events are immutable; commands are mutable requests that may produce 0..N events
- Snapshots exist for performance only, never correctness
- **Biggest pitfall for us: external effects on replay** — tool calls will re-fire unless replay-mode is a first-class flag

### 3.2 ReAct loop (Yao et al. 2022, arxiv 2210.03629)

Paper's loop: `Thought_i → Action_i → Observation_i` until `Finish[answer]`. Actions are strictly sequential tokens in the paper.

**Modern divergence** (Anthropic/OpenAI tool use, LangGraph):
1. **Parallel actions** — model emits N tool calls per turn; fan out; batched observations
2. **Thoughts are optional / implicit** — hidden reasoning channel or none at all
3. **Termination is structural** — absence of `tool_use` blocks, not a literal `Finish[]`

### 3.3 Event schema recommendations

| Event | When | Key fields |
|---|---|---|
| `RunStarted` | command accepted | model, params, system_prompt_hash, tool_registry_hash |
| `UserMessageAppended` | input arrives | content, attachments |
| `TurnStarted` | LLM call begins | turn_id, prompt_hash, params_snapshot |
| `AssistantMessageCompleted` | stream ends | turn_id, full_text, stop_reason, raw_response_hash |
| `ToolCallScheduled` | model requests | call_id, turn_id, tool, args |
| `ToolCallCompleted` \| `ToolCallFailed` | executor returns | call_id, result/error, duration_ms, attempt |
| `ReasoningEmitted` *(optional)* | extended thinking | turn_id, content (encryptable) |
| `BudgetExceeded` | budget trips | limit, actual, where |
| `RunCompleted` \| `RunFailed` \| `RunCancelled` | terminal | final_output, reason |

**Key decisions:**
- **Don't log per-token events in the canonical log.** Too much volume, couples log to provider chunking, hash chain thrashes. Stream tokens on a side pubsub channel keyed by turn_id.
- **Tool calls = always two events.** Scheduled + Completed/Failed. Non-negotiable for resume-after-crash, retry audit, idempotency.
- **Retries append, never mutate.** Each attempt is its own Completed/Failed event sharing call_id + attempt counter.
- **Reasoning events are optional and independently encryptable.**

### 3.4 The replay-under-LLM-nondeterminism question

Two coherent definitions, pick one:

**State-faithful replay** (LangGraph model): Replay restores recorded outputs from the log. LLM is never re-called during replay. Non-determinism is a non-issue. Trade-off: can't "what-if" with a different model.

**Re-execution replay** (Fowler's spirit): Replay re-runs handlers against recorded inputs. Requires either (a) determinism contracts (temp=0 + seed + pinned model + pinned backend — providers don't fully honor) or (b) a response cache keyed by `(prompt_hash, params_hash)`.

**Recommendation**: Default = state-faithful. Offer re-execution as opt-in mode with response cache. Record `model_id`, `params`, `seed`, `provider_request_id`, `raw_response_hash` on every LLM event so drift is detectable.

### 3.5 Sources

- [Fowler — Event Sourcing](https://martinfowler.com/eaaDev/EventSourcing.html)
- [Fowler — What do you mean by "Event-Driven"?](https://martinfowler.com/articles/201701-event-driven.html)
- [ReAct paper (arxiv 2210.03629)](https://arxiv.org/abs/2210.03629)
- [LangGraph persistence](https://docs.langchain.com/oss/python/langgraph/persistence)
- [LangGraph.js persistence guide](https://langgraphjs.guide/persistence/)
- [Debugging non-deterministic LLM agents with checkpoints](https://dev.to/sreeni5018/debugging-non-deterministic-llm-agents-implementing-checkpoint-based-state-replay-with-langgraph-5171)

---

## 4. Streaming cost enforcement

### 4.1 What providers give you mid-stream

- **Anthropic** — rich signal. `message_start.usage.input_tokens` arrives immediately (plus cache_creation / cache_read). `message_delta.usage.output_tokens` fires multiple times with **cumulative** output count. Use latest value, don't sum. This is the only provider that gives a billing-accurate running counter.
- **OpenAI** — set `stream_options: {include_usage: true}`. Usage is `null` on every chunk except the terminal. If you cancel mid-stream, **you don't get the final usage chunk** and forfeit OpenAI's authoritative count.
- **Gemini** — `usageMetadata` appears only on the last chunk. Intermediate chunks are nil.

**Implication**: Anthropic enforces exactly; OpenAI/Gemini require local tiktoken estimation until the final chunk lands (and may never land if cancelled).

### 4.2 Go HTTP cancellation gotchas

- `http.Request.WithContext(ctx)` ties connection lifetime to ctx — correct primitive
- **HTTP/2 stream leaks** on cancel ([golang/go#21229](https://github.com/golang/go/issues/21229), [#52853](https://github.com/golang/go/issues/52853))
- **HTTP/1.1 deadlocks on 1.22+** on cancel ([golang/go#65705](https://github.com/golang/go/issues/65705))
- Always `defer resp.Body.Close()` AND cancel context
- Treat `context.Canceled` as normal terminal, not error

### 4.3 Nobody in Go does this yet

- LiteLLM (Python): budgets enforced **before** call, not mid-stream
- LangChain/LangGraph: zero mid-stream enforcement; `get_openai_callback` returns zero in streaming mode
- langchaingo, Eino, ADK: no cost tracking at all
- Langfuse, LangSmith: observability only, post-hoc
- Gateway-level (agentgateway, envoy): proof-of-concept mid-stream enforcement exists but only as infra, not library

**Verdict**: this is a real gap in the Go library ecosystem.

### 4.4 Design approach

1. Unified `TokenUsage` struct populated per-provider:
   - Anthropic → `message_delta.usage` verbatim
   - OpenAI/Gemini → local tiktoken on accumulated deltas, every N chunks
2. Budget enforcer = goroutine watching a channel of usage updates. When `estimatedCost >= budget`, call context `cancel()`.
3. Enforcement lives **outside** the SDK call — SDK only sees a cancelled context.
4. Return structured `BudgetExceeded` error with `{input_tokens, output_tokens_estimated, partial_text, partial_tool_calls}`.
5. Document: OpenAI/Gemini mid-cancel counts are estimates; only Anthropic is billing-accurate.

### 4.5 Edge cases

- **Partial tool calls** — Anthropic streams tool args as `input_json_delta`. Cancel mid-tool = unparseable partial JSON. Either buffer-and-discard or expose as "aborted tool use" result.
- **Input vs output pricing** — input committed at `message_start`. Charge full prompt up front, decrement separate "output budget" as tokens stream.
- **Cache-read tokens** are ~10x cheaper than cache-writes. Price separately or reject cheap requests.
- **Cumulative-vs-incremental confusion** — classic bug ([agno#6537](https://github.com/agno-agi/agno/issues/6537)). `message_delta.usage.output_tokens` is cumulative — take latest, don't sum.

### 4.6 Sources

- [Anthropic streaming](https://platform.claude.com/docs/en/build-with-claude/streaming)
- [Anthropic fine-grained tool streaming](https://platform.claude.com/docs/en/agents-and-tools/tool-use/fine-grained-tool-streaming)
- [OpenAI include_usage announcement](https://community.openai.com/t/usage-stats-now-available-when-using-streaming-with-the-chat-completions-api-or-completions-api/738156)
- [Gemini streaming usage](https://discuss.ai.google.dev/t/how-can-i-track-token-usage-when-streaming-content-with-gemini/116526)
- [LiteLLM budget_manager.py](https://github.com/BerriAI/litellm/blob/main/litellm/budget_manager.py)
- [The $47K Agent Loop (dev.to)](https://dev.to/waxell/the-47000-agent-loop-why-token-budget-alerts-arent-budget-enforcement-389i)
- [Cloudflare — Go net/http timeouts](https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/)
- [Langfuse token & cost tracking](https://langfuse.com/docs/observability/features/token-and-cost-tracking)

---

## 5. Synthesis → Starling's positioning

From the research, four specific things nobody else does, in priority order of defensibility:

1. **Event log is the canonical state** — not bolt-on, not a session cache.
2. **Deterministic replay as CI-testable property** — ship a `Replayer` and make it a CI gate from M2 onward.
3. **Mid-stream cost enforcement** — Anthropic-first, OpenAI/Gemini via tiktoken.
4. **Hash-chained log for audit** — per-run BLAKE3 chain, optional Merkle root per run.

Everything else (tool interface, provider abstraction, ReAct loop) is table stakes — we must match Eino/ADK quality but not try to differentiate there.
