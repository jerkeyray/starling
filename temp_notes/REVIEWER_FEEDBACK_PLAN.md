# External-reviewer feedback: validation + prioritized update plan

## Context

A user of Starling (downstream consumer) submitted a long review. They're
the only outside consumer we know of, so each item is signal — but the
list is large and mixed-quality. This plan validates every point against
the current tree (HEAD = `main`, last commit `f1945a6`) and groups items
by **what the evidence shows** + **whether it's worth the work**.

The goal of the document is *triage*, not implementation. Each accepted
item ends with a short note on the change required. Concrete code changes
will be planned individually after the user picks the cuts.

### Decisions taken from triage discussion

- **Scope:** Tiers 1, 2, and 3 are all in. Tier 4 is dropped from this
  plan and parked as future-work issues.
- **Release posture:** stay in beta. Land *all* the work below first,
  then cut a single fresh `v0.1.0-beta.N` tag at the end. No staged
  beta tagging between tiers — tiers are organizational, not release
  boundaries.
- **Docs (Tier 2.6):** not blocking. No public push planned soon, so
  docs land progressively and don't gate the tag.

Repo facts used to validate (all checked):

- Tags: only `v0.1.0-beta.1` exists. No `v0.x.y` GA. No `CHANGELOG.md`.
- `cmd/starling-inspect/main.go` already exists (37 LOC, wraps
  `starling.InspectCommand(nil)`). Not blocked by code — only by release.
- `internal/merkle/merkle.go` is unexported. Used by `agent.go`,
  `eventlog/validate.go`, examples reach in via copy or skip it.
- No `starlingtest` package. Test helpers (scripted provider, memory log
  helpers, replay assertions) are duplicated per-package.
- `examples/m1_hello/main.go` is 296 LOC and bundles run/inspect/replay/
  reset/print into a single CLI — not a 50-line "first agent."
- No `docs/` directory. README is the entire prose surface.
- No `prompt` package, no `provider.RegisterPricing`, no
  `provider.ErrRateLimit/ErrAuth/ErrServer/ErrNetwork`, no `tool.Wrap`,
  no `eventlog.Fork`, no `Agent.YieldFor`, no `Session`, no `--version`,
  no `doctor` subcommand, no cross-run dashboard / diff view in inspector.
- Pricing is in `budget/prices.go` with a hardcoded map; the `unknown
  model → $0 + warn-once` path the reviewer hit is exactly as described
  (`budget/prices.go:55`).
- `RunSummary` (`eventlog/eventlog.go:21`) has `RunID, StartedAt,
  LastSeq, TerminalKind` — no cost/tokens, so dashboard totals require
  aggregation work, not just a UI change.
- `Agent.Stream` (`stream.go`) emits `StepEvent` (CBOR-flavored), not the
  typed `TextDelta/ToolCallStarted/Done` the reviewer wants.
- `tool.ErrPanicked`/`ErrTransient` already exist (`tool/tool.go:26,37`)
  — the reviewer's framing of those as "missing" is wrong; they meant
  the *tool lifecycle reference doc*.
- Inspector: `handleRuns` already supports `?status=` and `?q=`
  (`inspect/handlers.go:24-26`), but only on minimal `RunSummary` fields.

---

## Tier 1 — release-blocking foundation

These either have direct evidence the reviewer hit a footgun, or they
unblock the rest of the list. None require speculative design work.
The downstream consumer can drop `replace` once the new beta tag is
cut at the end of all this work.

### 1.1  CHANGELOG.md + beta release policy
- **Validation:** confirmed. No `CHANGELOG.md` exists. Only
  `v0.1.0-beta.1` is tagged; downstream cannot `go install` or drop
  `replace` against an unpinned `main`. No GA push planned, so this
  is a beta-cadence problem, not a semver-1.0 problem.
- **Action:** create `CHANGELOG.md` (Keep-a-Changelog format) with
  a stub `[Unreleased]` block. Document beta-cadence policy in the
  README: each beta tag is consumable; breaking changes allowed
  between betas with changelog notes; no compatibility promise until
  GA. The actual tag (`v0.1.0-beta.2` or whatever the user picks) is
  cut at the very end, by the user, once everything else lands.

### 1.2  SchemaVersion contract
- **Validation:** `event.SchemaVersion` is checked at resume
  (`resume.go:295`) but the *meaning* of a bump (compat / migration /
  replay) is not written down.
- **Action:** add a short "Schema versioning" section to README
  defining: when SchemaVersion bumps; what consumers must do on bump;
  that minor bumps must remain resume-compatible; that migration tools
  live in `migrate_command.go`. Pairs with CHANGELOG (1.1).

### 1.3  Export `merkle` package
- **Validation:** `internal/merkle/merkle.go` is import-blocked.
  Reviewer correctly identifies that opening it lets third parties
  write their own event producers without copying the chain logic.
  Files that touch it: `agent.go`, `eventlog/validate.go`,
  `event/types.go`, `bench/replay_test.go`, `eventlog/contract_test.go`.
- **Action:** move `internal/merkle` → `merkle/`. Update the ~6 import
  paths. No API change beyond visibility.

### 1.4  `starlingtest` package
- **Validation:** confirmed; helpers duplicated across `m1_hello`,
  `step/llm_test.go`, `agent_test.go`, `replay_test.go`. None are
  exported.
- **Action:** create `starlingtest/` with three exports:
  - `ScriptedProvider` (replaces ad-hoc scripted providers in tests)
  - `MemoryLog`-builder helpers (e.g. seeded `RunStarted`, terminal
    appenders)
  - `AssertReplay(t, agent, log, runID)` for the replay-divergence
    test loop
  Migrate existing internal tests to use it; this both validates the
  surface and removes duplication.

### 1.5  `eventlog.Fork(srcPath, dstPath, beforeSeq) error`
- **Validation:** confirmed missing. Reviewer's WAL footgun is real —
  `runs.db-shm` and `runs.db-wal` exist in the repo right now
  (uncommitted, but they always exist after a run on SQLite WAL).
  A naïve `cp` of just `.db` produces a corrupt copy.
- **Action:** add `eventlog.Fork` to the package, implemented per
  backend. SQLite version uses `VACUUM INTO` (handles WAL correctly),
  then deletes events with `seq >= beforeSeq` for the affected runs.
  Postgres + memory implementations follow the same shape.
  Cookbook entry (Tier 2) documents the API.

### 1.6  `provider.RegisterPricing(model, in, out, cacheRead, cacheCreate float64)`
- **Validation:** confirmed missing. `budget/prices.go` has a private
  map with no override hook; the warn-once path
  (`budget/prices.go:60`) is the only feedback channel.
- **Action:** add `budget.RegisterPricing` (or `provider.RegisterPricing`
  that delegates) — register/override entries in the existing `prices`
  map under a `sync.RWMutex`. Reset the warn-once memo for the model
  when overridden so a stale warning doesn't outlive the fix.

### 1.7  Structured provider errors
- **Validation:** confirmed missing at the `provider/` level. Each
  provider currently surfaces raw upstream errors. Tool-level
  `ErrTransient/ErrPanicked` already exist; this is the matching set
  one layer up.
- **Action:** add `provider.ErrRateLimit`, `ErrAuth`, `ErrServer`,
  `ErrNetwork` as sentinel errors in `provider/provider.go`. Wrap from
  each adapter (`anthropic`, `openai`, `gemini`, `bedrock`, `openrouter`)
  using their existing HTTP-status branching. Don't change agent retry
  policy yet — the value is `errors.Is` for downstream consumers.

### 1.8  `--version` on `starling` and `starling-inspect`
- **Validation:** confirmed missing in `cmd/starling/main.go` and
  `cmd/starling-inspect/main.go`.
- **Action:** add a top-level `Version` constant in package `starling`
  (also surfaces in `RunStarted`?). Wire `--version` into both binaries.
  Trivial; bundle with the release.

---

## Tier 2 — second wave (still pre-tag)

These are real gaps but require either design work or non-trivial
implementation. They ship as part of the same eventual beta cut;
tiers split the work, not the release. Docs (2.6) are non-blocking
and may continue past the tag without holding it up.

### 2.1  Cross-run dashboard + per-run totals in inspector
- **Validation:** confirmed. `RunSummary` carries no cost/tokens —
  the inspector currently can't render totals without aggregating
  events on the fly.
- **Action:** extend `RunSummary` with `EventCount, CostUSD,
  TokensIn/Out, CacheHitRatio, DurationMs`. Backends compute on
  `ListRuns` (acceptable cost: SQLite already streams events). Inspector
  `runs.html` gets a totals header + per-row chips. Sort/filter ride
  the existing `?q=`/`?status=` plumbing.
- **Drag-along:** "summary header on run page" (reviewer's separate
  bullet) is the same data, scoped to one run.

### 2.2  Inspector run-diff view
- **Validation:** reviewer ranks this as the #1 gap. No `Diff/Compare`
  helpers exist in `inspect/` today.
- **Action:** new `/diff?a=<runID>&b=<runID>` page. Diff is at
  event level: align by sequence, render side-by-side, highlight
  payload deltas (CBOR → canonical JSON via existing
  `prettyJSON`). Reuse `replay/stream.go` divergence detection where
  possible.

### 2.3  Inspector: replay divergence feedback
- **Validation:** confirmed. `dispatchReplaySession` and the SSE
  channel exist (`inspect/replay.go`); UI has no toast on divergence.
- **Action:** add a small toast component fed by the existing SSE
  stream. Same plumbing already carries the divergence event; just
  needs a UI sink.

### 2.4  Cross-provider replay refusal
- **Validation:** `RunStarted` records `ProviderID`/`ModelID`
  (`agent.go:380` area). Replay does not check today.
- **Action:** in `replay.RunReplay`, compare recorded
  `ProviderID/ModelID` to the live agent's; refuse unless they match
  or `--force` is set on the CLI. Wire into `replay_command.go`.

### 2.5  `RunResult.CacheStats`
- **Validation:** cache stats are emitted in usage events but not
  rolled into `RunResult` (`result.go:13`).
- **Action:** add `CacheStats{Hits, Misses, ReadTokens, CreateTokens}`
  to `RunResult`. Populate in `Agent.buildResult` (`agent.go:512`)
  by walking the existing usage-event records.

### 2.6  Docs site (or `docs/` directory) — non-blocking
- **Validation:** there is no `docs/`. README is the only prose.
- **Status:** does not block the beta tag. Land in waves, in this
  order; later waves can land after the tag. Wave A
  (`getting-started.md`, `mental-model.md`) is the highest payoff
  and smallest writing effort, so try to fit it in pre-tag.
- **Action:** add a `docs/` tree with the following pages, in order:
  1. `getting-started.md` — 50-line "hello agent" (paired with §2.7)
  2. `mental-model.md` — Run lifecycle, terminal events, when to use
     one Run vs many. Anchors the reviewer's PRD §4.4 confusion.
  3. `cookbook/` — `branching.md`, `forking.md`, `manual-writes.md`,
     `multi-turn.md`, `testing-without-llms.md` (uses
     `starlingtest`).
  4. `reference/` — `events.md` (every Kind, payload, Annotations
     key), `step-primitives.md` (`Now/Random/SideEffect`),
     `cost-model.md` (pricing + cache + `RegisterPricing`),
     `tools.md` (lifecycle, errors, retries), `replay.md`
     (extracted from package doc-comments), `metrics.md` (Prom +
     OTel names + where they're emitted), `save-file.md`
     (SQLite schema, CBOR layout).
  5. `faq.md` — answers reviewer's listed questions.
- **Drag-along:** moves the "manual-writer pattern", "multi-turn",
  "branching/forking" examples into prose form (Tier 3 items 3.1–3.3
  are the executable companions, not separate docs).

### 2.7  `examples/hello/` — the 50-line first-agent
- **Validation:** confirmed; m1_hello is a Swiss army knife.
- **Action:** new `examples/hello/main.go`, ~50 LOC: load env,
  build provider, build Agent with one tool, `Run`, print result.
  Keep `m1_hello` as the comprehensive example. README links the
  hello example as the entry point.

---

## Tier 3 — ship opportunistically (still pre-tag)

In scope. Each is small enough to slot in alongside whichever Tier-1
or Tier-2 item it sits next to. The cheap wins (3.7, 3.10, 3.11)
bundle naturally with Tier 1; the rest follow Tier 2.

- **3.1 Cookbook example: branching** — pairs with `eventlog.Fork`
  (1.5). Just an `examples/branching/` directory plus the cookbook
  page from 2.6.
- **3.2 Cookbook example: manual event writing** — uses the newly
  exported `merkle` package (1.3) to demonstrate writing without
  `Agent.Run`.
- **3.3 Cookbook example: multi-turn conversation** — concrete
  illustration of "one Run per turn vs one Run total." Backs the
  mental-model page.
- **3.4 `tool.Wrap(t, ...Middleware)`** — useful for logging/timing/
  auth without re-implementing `tool.Tool`. Small surface: function
  composition over `Execute`.
- **3.5 `Agent.RunStream` typed events** — add typed wrapper events
  (`TextDelta, ToolCallStarted, ToolCallEnded, Done`) layered over
  the existing `StepEvent` channel; do not replace the CBOR stream.
- **3.6 Search & filter chips in inspector** — extend the existing
  `?q=` to payload substring matching; add chips ("only failed",
  "only with tool calls", "last hour") as preset queries.
- **3.7 `inspect <too many args>` error message** — five-minute fix.
  Bundle whenever someone touches `cmd/starling-inspect/main.go`.
- **3.8 `starling doctor`** — env validation, save-file validation,
  schema-version compatibility report. Trivial wrapper over existing
  `validate` and `schema-version` commands plus an env-var check.
- **3.9 Inline annotations on inspector events** — once 2.1 lands, the
  data is cheap to slot into `event_row.html`.
- **3.10 `tool.Typed` panic message** — replace generic message with
  the reviewer's wording. One-line patch in `tool/typed.go`.
- **3.11 `golangci-lint` config** — drop in a `.golangci.yml`.

---

## Tier 4 — out of scope for this plan

Per the triage decision, these are dropped from the current update.
Listed here for visibility / future tracking.

- **`starling.NewProviderFromEnv()`** — looks like sugar but the
  semantics ("which env var? what fallback? merge with explicit
  config?") are not obvious. Defer until 2+ consumers ask.
- **`starling.NewApp(spec)`** — overlaps with the existing dual-mode
  pattern shown in `m1_hello`. Wait for evidence the boilerplate is
  actually painful, not just visible.
- **`Session` primitive (N Runs + shared budget)** — design work,
  unclear interaction with `RunResult` and per-run hashing. Capture
  in an issue; punt on implementation.
- **`Agent.YieldFor("user_input")`** — HITL pause is a real need but
  cuts across replay, eventlog, and resume. Worth its own design
  document. Defer.
- **`prompt.Template` package** — speculative. Most consumers do fine
  with `fmt.Sprintf`; building a templating system is a separate
  product decision. Decline for now.
- **Per-tool budgets within Run-level budget** — useful but adds
  complexity to `budget/`. Defer until requested again.
- **Inspector: gantt timeline, token mini-charts, live auto-scroll** —
  visual polish; not blocking adoption. Park.
- **Anatomy of save file doc** — covered well enough by
  `docs/reference/save-file.md` in 2.6; no separate effort.

---

## Critical files (Tier 1 only)

- `cmd/starling/main.go` — `--version`
- `cmd/starling-inspect/main.go` — `--version`
- `internal/merkle/merkle.go` → `merkle/merkle.go` (rename + import
  rewrites in: `agent.go`, `eventlog/validate.go`,
  `eventlog/contract_test.go`, `event/types.go`,
  `bench/replay_test.go`, `examples/m4_inspector_demo/main.go`)
- `budget/prices.go` — add `RegisterPricing`, mutex, warn-memo reset
- `provider/provider.go` — sentinel errors
- `provider/{anthropic,openai,gemini,bedrock,openrouter}/*.go` — wrap
  HTTP-status errors with the new sentinels
- `eventlog/eventlog.go` — `Fork` interface method
- `eventlog/sqlite.go` — `Fork` via `VACUUM INTO`
- `eventlog/postgres.go` — `Fork` via `pg_dump`/copy or schema-level
  approach
- `eventlog/memory.go` — `Fork` via deep copy
- `starlingtest/` (new package) — scripted provider, log helpers,
  replay assertions
- `CHANGELOG.md` (new)
- `README.md` — schema-versioning policy section, install instructions

## Verification

Tier 1 is testable end-to-end:

1. `go test ./...` after each change; the existing test suite
   exercises every touched path (Fork would need new tests).
2. `starlingtest` migration: every test file that loses its private
   helpers must still pass.
3. Local smoke: build `cmd/starling-inspect`, run against `runs.db`
   in repo root, confirm UI loads and `--version` prints.
4. Fork: write a SQLite log with N events, fork at `beforeSeq`, open
   destination read-only, confirm event count = beforeSeq-1 and that
   `runs.db-shm`/`runs.db-wal` were not silently produced/leaked at
   the destination.
5. Pricing: `budget.RegisterPricing("custom-model", ...)`, then
   `budget.CostUSD("custom-model", 1000, 1000)` returns the registered
   cost (not 0); the warn-once stderr line does not fire.
6. Provider errors: pointer test rigging a 429 response from the
   Anthropic adapter and asserting `errors.Is(err,
   provider.ErrRateLimit)`.
7. Tag a fresh beta after *all* of the above is in (`v0.1.0-beta.2`
   or whatever the user picks). Per CLAUDE.md, the user runs
   `git tag` and `git push --tags` themselves — this plan never
   tags or pushes autonomously.
