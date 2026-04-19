# Starling — M2 Implementation Plan

> M2 exit criteria (PRD §8): Run an agent. Kill the process. Call
> `Replay(runID)`. Agent replays with identical output.
> Target: weeks 8–11 (4 weeks — one week slack vs PRD's 3).
> M1 shipped in-memory log + OpenAI-compatible provider; M2 adds the
> replay story, durable storage, parallel tools, a second provider,
> and full budget enforcement.
> Last updated: apr 2026

## Status — what shipped

M2 is code-complete. Tagged `v0.1.0-alpha`.

| Task | Status |
|---|---|
| T14 — TurnID threading | ✓ done |
| T15 — determinism helpers (Now/Random/SideEffect) | ✓ done |
| T16 — SQLite `EventLog` backend | ✓ done |
| T17 — replay verifier + `ErrNonDeterminism` | ✓ done |
| T18 — parallel tool dispatch (`step.CallTools`) | ✓ done |
| T19 — Anthropic provider (thinking + caching) | ✓ done |
| T20 — full budget enforcement (output_tokens / USD / wall-clock) | ✓ done |
| T21 — retry / backoff for idempotent tool calls | ✓ done |
| T22 — Postgres `EventLog` backend | deferred (stretch; first to cut) |
| T23 — docs, reshuffle, release | ✓ done (this task) |

Known follow-ups carried into M3:
- Replay of a mid-stream budget trip needs `replay/provider.go`
  `buildTurns` to tolerate a trailing `TurnStarted` with no
  `AssistantMessageCompleted`.
- Retry event trail lands contiguously in the log, but per-attempt
  stats aren't yet surfaced on `RunCompleted`.

The original task breakdown is preserved below unchanged as the
historical record of what was planned vs. what landed.

---

---

## M2 scope

**In scope:**
- Replay verifier (`replay.Verify`) with non-determinism detection
- Deterministic helpers: `step.Now`, `step.Random`, `step.SideEffect`
- Durable `EventLog` backend: SQLite (single-file embedded)
- Parallel tool dispatch (`step.CallTools`)
- Anthropic provider (real, not stub) with reasoning-block normalization
- Full budget enforcement: output tokens, USD, wall-clock with
  mid-stream interrupt
- Retry / backoff / idempotency keys on `step.CallTool`
- Expose minted TurnID on `provider.Response`
- First tagged pre-release: `v0.1.0-alpha`

**Stretch (drop first if the milestone slips):**
- Postgres `EventLog` backend

**Out of scope (deferred to M3):**
- Streaming cost budget mid-flight cancellation with refund accounting
- Gemini / Bedrock providers
- OpenTelemetry spans
- Context-window truncation strategies
- MCP tool interface (M4)

---

## Task breakdown

Every task lists its dependencies, deliverable, and exit test. Order
matters — tasks assume prior tasks complete.

### T14 — surface TurnID on provider.Response (0.5 day)

**Depends on:** M1 complete.

**Problem:** `step.LLMCall` mints a TurnID (ULID), writes it into
`TurnStarted`, but never returns it. The agent loop passes `""` into
`step.CallTool`, so `ToolCallScheduled.TurnID` is empty. Replay can't
correlate tool calls back to their turn without a seq walk.

**Deliverable:**
- Add `TurnID string` field to `provider.Response`.
- `step.LLMCall` populates it.
- `agent.go`'s `react` passes `resp.TurnID` into `step.CallTool`.
- Delete the `currentTurnID(ctx) string` placeholder.

**Exit test:** every `ToolCallScheduled` emitted by `agent_test.go`'s
`TestAgent_ToolRoundTrip` carries a non-empty TurnID equal to the
preceding `TurnStarted.TurnID`.

---

### T15 — step.Now / step.Random / step.SideEffect (1.5 days)

**Depends on:** T14.

**Problem:** Tools that read the wall clock (`current_time`) or make
HTTP calls are non-deterministic. Replay needs to record their outputs
and play them back. The hook points live in `step/` per
ARCHITECTURE.md §6.

**Deliverable:**
- `step/ndet.go`:
  - `Now(ctx) time.Time` — on first call per run, records a
    `SideEffectRecorded{Kind:"now", Value: <unix-nanos CBOR>}` and
    returns it. Under replay (detected via a flag on `step.Context`)
    replays the recorded value.
  - `Random(ctx, seed int64) *rand.Rand` — records the seed, returns
    a `rand.Rand` seeded with it.
  - `SideEffect[T any](ctx, key string, fn func() (T, error)) (T, error)` —
    first call records the result; replay returns the recorded
    result without invoking `fn`.
- `step.Context` gains a `mode` enum (`Live` | `Replay`) + a
  `replayCursor` that tracks position in the recorded event stream.
- All three helpers use `SideEffectRecorded` (event kind 9 already
  exists per EVENTS.md) plus a `Key string` field for correlation
  across replay.

**Exit test:**
- Live run: call `step.Now(ctx)` three times → three
  `SideEffectRecorded` events with distinct values.
- Replay run over that same log: `step.Now(ctx)` returns the same
  three values without re-reading the wall clock (use a test
  `clockFn` injection to assert it's not called).

---

### T16 — SQLite EventLog backend (2 days)

**Depends on:** M1 (the interface is frozen).

**Deliverable:**
- `eventlog/sqlite.go` — `NewSQLite(path string, opts ...Option) (EventLog, error)`.
  Single table `events(run_id TEXT, seq INTEGER, prev_hash BLOB, ts INTEGER, kind INTEGER, payload BLOB, PRIMARY KEY(run_id, seq))`.
  Dependency: `modernc.org/sqlite` (pure-Go, no cgo).
- WAL mode on by default; `PRAGMA synchronous=NORMAL`.
- `Append`: single-statement `INSERT` inside a transaction that also
  re-reads the last row for the run under `BEGIN IMMEDIATE` to
  enforce the same Seq/PrevHash invariants as the in-memory backend.
  Reuse `validateAppend` logic — lift it from `memory.go` into an
  unexported `sharedValidate` helper.
- `Read`: `SELECT ... ORDER BY seq`.
- `Stream`: polling-based to start (poll every 50ms); document that
  subscribers can fall seconds behind under load. Real pub/sub via
  SQLite notify channels is M3.
- `Close`: closes the DB handle.

**Exit test:**
- `go test -race ./eventlog -run SQLite -count=3` passes.
- Round-trip: run the agent against `NewSQLite("test.db")`,
  close/reopen, `Validate` still passes.
- Tamper test: `UPDATE events SET payload = X'00' WHERE seq = 3`,
  `Validate` rejects.

---

### T17 — replay.Verify (3 days)

**Depends on:** T14, T15, T16.

**Deliverable:**
- `replay/replay.go`:
  - `Verify(ctx, log EventLog, runID string, a *Agent) error` —
    re-executes the run in replay mode and asserts every emitted
    event matches the recorded one byte-for-byte. Mismatch →
    `starling.ErrNonDeterminism` with details.
- Mechanics:
  - Read stored events; initialize a `step.Context` in replay mode
    with the recorded event stream.
  - Swap the provider for a `replayProvider` that reads
    `AssistantMessageCompleted` / `ReasoningEmitted` / tool-use
    planning from the recorded events and re-emits the matching
    `StreamChunk` sequence. No network.
  - Swap the step emitter for one that, before appending, compares
    the would-be event against the recorded event at the same seq
    and errors on payload/kind mismatch.
  - Tools run for real; their outputs are then compared against the
    recorded `ToolCallCompleted.Result`. (We'll revisit this in M3 —
    a strict replay would short-circuit the tool too, using the
    recorded result as ground truth. For M2, running + checking
    catches code drift.)
- Exposed from root as `starling.Replay(ctx, log, runID) error`.

**Exit test:**
- `go test ./replay -count=5` passes.
- Integration: run `TestAgent_ToolRoundTrip` against a canned
  provider, persist the log to SQLite, close, re-open,
  `starling.Replay(...)` returns nil.
- Negative: after replay-run, mutate the echo tool to return a
  different value; replay now returns `ErrNonDeterminism`.

---

### T18 — parallel tool dispatch (1 day)

**Depends on:** T14.

**Deliverable:**
- `step/tools.go` — add `CallTools(ctx, calls []ToolCall) ([]ToolResult, error)`.
  Uses `golang.org/x/sync/errgroup`; fan-out with a semaphore
  (default 8, configurable via `step.Config.MaxParallelTools`).
- `agent.go` — when a response contains >1 tool use, dispatch via
  `CallTools` instead of the sequential loop.
- Event ordering: preserve dispatch-order in the log by minting
  CallIDs upfront and using a `sync.Map[CallID] *pending` so
  `ToolCallScheduled` fires in the same order as the provider's
  `ToolUses` slice. `ToolCallCompleted` can land in any order — seq
  numbers reflect completion order, which is the deterministic
  ground truth for replay.

**Exit test:**
- Two slow tools (500ms each) complete in ~500ms, not 1s.
- `Validate` still passes after a parallel run.
- Replay of a parallel run matches the original seq order.

---

### T19 — Anthropic provider (real) (3 days)

**Depends on:** M1.

**Deliverable:**
- `provider/anthropic/anthropic.go` — replace the current stub with a
  real streaming adapter against `github.com/anthropics/anthropic-sdk-go`.
- Normalize Anthropic's event stream (`message_start`,
  `content_block_start/delta/stop`, `message_delta`, `message_stop`)
  into Starling's `StreamChunk` shape. Reasoning blocks (extended
  thinking) → `ChunkReasoning`.
- Handle tool-use blocks: `content_block_start` with
  `type:"tool_use"` → `ChunkToolUseStart`; subsequent
  `input_json_delta` → `ChunkToolUseDelta`; `content_block_stop` →
  `ChunkToolUseEnd`.
- Cost table entries for `claude-3-5-sonnet-*`, `claude-3-5-haiku-*`,
  `claude-opus-*` in `budget/prices.go`.

**Exit test:**
- Unit tests with canned SSE transcripts (captured from the SDK).
- Manual: `examples/m1_hello` with `ANTHROPIC_API_KEY` (a
  `main.go` switch driven by env var — lands as a small edit to
  the demo or a new `examples/m2_anthropic` sibling).

---

### T20 — full budget enforcement (1.5 days)

**Depends on:** T17 (because budget-triggered terminations must replay).

**Deliverable:**
- `budget/enforce.go` — `Enforce(cfg Budget, usage UsageUpdate, wallStart time.Time) error`.
- `step/llm.go`: after every `ChunkUsage`, call `Enforce`. On trip,
  emit `BudgetExceeded{Limit, Where:"mid_stream", Cap, Actual}`,
  close the provider stream, return `ErrBudgetExceeded`.
- Wall-clock: a goroutine watches the run's start time; on deadline,
  cancels the per-run context. `ErrBudgetExceeded` races with
  `context.DeadlineExceeded`; both resolve to
  `BudgetExceeded{Limit:"wall_clock"}` in the terminal.

**Exit test:**
- `MaxOutputTokens=5`, model emits 20: `BudgetExceeded.Actual > 5`,
  run terminates with `RunFailed{ErrorType:"budget"}`.
- `MaxUSD=0.0001`, run with any non-trivial prompt trips it.
- `MaxWallClock=50ms` against a slow fake provider → trips within
  100ms.

---

### T21 — retry / backoff on step.CallTool (1 day)

**Depends on:** T14.

**Deliverable:**
- `step.ToolCall` gains `Idempotent bool`, `MaxAttempts int` (default
  1), `Backoff func(attempt int) time.Duration` (default exponential
  with jitter).
- On transient errors (timeout, specific sentinels from the tool),
  retry. Each retry emits a fresh `ToolCallScheduled` /
  `ToolCallFailed` pair with `Attempt: n`.
- Final success → `ToolCallCompleted` with the final attempt number.
- Final failure → last `ToolCallFailed.Attempt == MaxAttempts`.

**Exit test:**
- Flaky tool (fails twice, succeeds on 3rd): log has 2× Failed +
  1× Completed; `CallTool` returns success.
- Non-idempotent tool: `MaxAttempts` ignored; first failure is
  final.

---

### T22 — Postgres EventLog backend (stretch) (2 days)

**Depends on:** T16.

**Deliverable:**
- `eventlog/postgres.go` — `NewPostgres(dsn string) (EventLog, error)`.
  Schema mirrors SQLite. Streaming via `LISTEN/NOTIFY` on a trigger
  that fires on INSERT (cheap, native, no polling).
- CI skips Postgres tests when `POSTGRES_DSN` unset.

**Exit test:** `POSTGRES_DSN=... go test -race ./eventlog -run Postgres`
passes.

---

### T23 — docs, reshuffle, release (1 day)

**Depends on:** everything else.

**Deliverable:**
- Move `temp_notes/*.md` → `docs/` (tracked this time).
- Update `README.md` with replay snippet + durable-log snippet.
- Add `docs/M2_PLAN.md` (this doc, updated to reflect reality).
- Write `docs/REPLAY.md` — user-facing replay cookbook.
- Tag `v0.1.0-alpha`.

**Exit test:** `go doc github.com/jerkeyray/starling/replay` shows a
coherent Verify doc. README walks someone from `go get` to a replay
in under 10 minutes.

---

## Milestone exit criteria

Check all of:

- [ ] `go test ./... -race -count 3` passes
- [ ] Agent run persists to SQLite; close + reopen preserves the log
- [ ] `starling.Replay` succeeds on a real run
- [ ] Replay with a mutated tool returns `ErrNonDeterminism`
- [ ] Two-tool turn fans out in parallel; replay matches
- [ ] Anthropic provider passes the same agent tests as OpenAI
  (swap Provider, same test body)
- [ ] All four budget axes trip cleanly with matching
  `BudgetExceeded` events
- [ ] `starling.ToolCall{Idempotent:true, MaxAttempts:3}` retries
  recorded as separate events
- [ ] `v0.1.0-alpha` tagged, CHANGELOG.md seeded

---

## Rough calendar

| Week | Tasks | Output |
|---|---|---|
| Week 8 | T14, T15, T16 | TurnID, determinism, SQLite |
| Week 9 | T17 | Replay works |
| Week 10 | T18, T19 | Parallel tools, Anthropic |
| Week 11 | T20, T21, T23 (T22 if time) | Budgets, retry, release |

~3 days slack across 4 weeks. T17 is the schedule risk — if replay
ergonomics surface surprises, T22 (Postgres) is the first thing cut.

---

## Open questions to resolve during M2

1. **Replay verifier strictness** — do we short-circuit tools with
   recorded results, or run them live and compare? Starting with run +
   compare (catches code drift); decide in T17 whether strict mode
   is worth a flag.
2. **SQLite driver** — `modernc.org/sqlite` (pure Go) vs
   `mattn/go-sqlite3` (cgo, faster). Pick `modernc` in T16; revisit
   if write throughput is the bottleneck.
3. **Mid-stream budget UX** — when wall-clock trips, do we try to
   let the current chunk complete? M2: hard cancel. M3: graceful
   drain if it's trivial.
4. **Anthropic reasoning** — record reasoning content or just its
   hash? M2: record content; revisit when users complain about log
   size.
5. **v0.1.0-alpha surface** — freeze or leave room to move? Leave
   room; the "alpha" suffix is the carve-out. Breaking changes still
   allowed until v0.1.0 proper.
