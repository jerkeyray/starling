# Changelog

All notable changes to Starling are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
the **beta-cadence** policy described in
[the README](README.md#release-policy).

While the project is in `v0.x.y-beta.N`, breaking changes between betas
are allowed and will be called out under the relevant tag. There is no
compatibility promise until the first GA (`v1.0.0`) tag.

## [Unreleased]

### Added

- `CHANGELOG.md` and a documented beta-cadence release policy in the
  README, plus an "Event log schema" section explaining what
  `event.SchemaVersion` bumps mean and how to migrate.
- `merkle` package: the BLAKE3 Merkle helpers used by the runtime are
  now public (`github.com/jerkeyray/starling/merkle`). Third parties
  writing their own event producers can reuse the chain implementation
  rather than copying it. Previously `internal/merkle`.
- `starlingtest` package: deterministic test scaffolding for downstream
  consumers. Ships `ScriptedProvider`, `NewStream`, `AppendRunStarted`,
  `AssertReplayMatches`, and `AssertReplayDiverges`. Internal tests
  migrated to use it; per-test `cannedProvider` shims removed.
- `eventlog.ForkSQLite`: WAL-safe SQLite branch helper. Copies a log
  via `VACUUM INTO` (so `.db-wal`/`.db-shm` are not silently leaked)
  and truncates events at the requested sequence boundary.
- `budget.RegisterPricing`: register or override per-model USD pricing
  at runtime. Resets the unknown-model warn-once memo so a stale
  warning doesn't outlive the registration.
- `provider.ErrRateLimit`, `ErrAuth`, `ErrServer`, `ErrNetwork` plus
  `WrapHTTPStatus` and `ClassifyTransport` helpers. All five built-in
  adapters (Anthropic, OpenAI, Gemini, Bedrock, OpenRouter) now wrap
  their SDK errors so callers can write retry policy via `errors.Is`.
- `starling.Version` constant and `--version` / `-v` / `version`
  arguments on both `cmd/starling` and `cmd/starling-inspect`.
- `RunResult.CacheStats` (`Hits`, `Misses`, `ReadTokens`,
  `CreateTokens`) aggregated from per-turn cache token counts.
- `Replay` refuses to run when the agent's
  `Provider.ID`/`APIVersion`/`Config.Model` disagree with the
  recording's `RunStarted`. Returns `ErrProviderModelMismatch`;
  override with `WithForceProvider()` (or `--force` on
  `starling replay`). The check trips before any turn executes.
- `RunSummary` carries per-run aggregates (`TurnCount`,
  `ToolCallCount`, `InputTokens`, `OutputTokens`, `CostUSD`,
  `DurationMs`) so dashboards no longer have to re-aggregate event
  streams. Computed on `ListRuns` for every backend.
- Inspector dashboard: totals strip above the runs table and a
  per-row breakdown (events, turns, tools, tokens, cost, duration);
  per-run page gets the same totals header.
- Inspector run-diff view at `/diff?a=<runID>&b=<runID>` aligns two
  runs by sequence number, renders payloads side-by-side, and
  surfaces the first divergence. New "Diff" link in the topbar.
- Inspector replay divergence toast: when a `Diverged=true` step
  arrives over the SSE stream, an auto-dismissing banner flashes so
  the user sees it even if they're scrolled away from the row.
- `examples/hello/` — minimal ~50-line first-agent.
- `docs/` directory: `getting-started.md` and `mental-model.md`
  (Wave A). More waves to follow.
- `tool.Wrap(t, ...Middleware)` — compose middleware around a tool's
  Execute without re-implementing `tool.Tool`. Outer middleware runs
  first; short-circuiting middleware can skip inner layers entirely.
- `Agent.RunStream` — typed AgentEvent stream
  (`TextDelta`, `ToolCallStarted`, `ToolCallEnded`, `Done`) layered
  over the existing `Stream`. Always closes after a single `Done`.
- `cmd/starling doctor` — quick health check covering binary
  version, provider env vars, schema version, and chain validation
  on a supplied SQLite log.
- Inspector preset chips on the runs dashboard: "with tool calls"
  and "last hour" alongside the existing status tabs.
- Inline metric annotation on inspector timeline rows
  (cost / tokens / cache ratio) for `AssistantMessageCompleted`.
- Cookbook examples + docs: branching (paired with
  `eventlog.ForkSQLite`), manual event writing (paired with the
  exported `merkle` package), and multi-turn conversations (one Run
  per user message). New runnable directories under `examples/` and
  doc pages under `docs/cookbook/`.
- `tool.Typed` panic message now states "In must be a struct; got X
  — top-level tool inputs must be JSON objects" with a wrap-it
  suggestion.
- `inspect <too many args>` returns a precise error instead of the
  misleading "missing <db> argument".
- `docs/` Wave B: full reference pages (`events.md`,
  `step-primitives.md`, `cost-model.md`, `tools.md`, `replay.md`,
  `metrics.md`, `save-file.md`) plus `docs/faq.md`. README and
  `docs/README.md` indices updated to match.

## [v0.1.0-beta.1] - 2026-04-30

First public beta tag. Subsequent betas will record their deltas
relative to this baseline; the contents of the v0.1.0-beta.1 cut
itself are recoverable from `git log v0.1.0-beta.1`.

[Unreleased]: https://github.com/jerkeyray/starling/compare/v0.1.0-beta.1...HEAD
[v0.1.0-beta.1]: https://github.com/jerkeyray/starling/releases/tag/v0.1.0-beta.1
