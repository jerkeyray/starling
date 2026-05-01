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

### Inspector UI revamp

- **Dark mode by default**, with a theme toggle (☀/🌙) in the
  topbar. The choice persists in `localStorage`; without one, the
  page follows `prefers-color-scheme`. A pre-paint script in
  `layout.html` applies the persisted value before the first frame
  to avoid a theme flash.
- New token system in `app.css`: 12-step neutral scale plus accent
  and four semantic colors, defined for both palettes. Every
  surface, border, badge, and chip resolves from the tokens — no
  more hard-coded hex.
- Topbar polished: brand glyph, active-page underline on
  Runs/Diff, an open-DB context chip (basename, hover for full
  path), theme toggle.
- Filter inputs gained a leading magnifier icon (runs page and the
  per-run timeline filter); buttons unified on a single
  token-driven shape; `tab.active` and `chip.active` are distinct.
- Totals strip restyled with subtle gradient and stronger numeric
  hierarchy; the per-run page reuses the same component.
- Run detail pane:
  - **Server-side JSON syntax highlighting.** New `highlightJSON`
    renders `.tok-key`, `.tok-string`, `.tok-num`, `.tok-bool`,
    `.tok-null`, `.tok-punct` spans; CSS colors them. No
    client-side parser, no JS dependency added.
  - **Sticky meta header** inside the pane — kind, hash, prev,
    call stay visible while the payload scrolls.
  - **Click-to-copy** on hash readouts and the run-id heading.
    Inline copies fire a transient toast; explicit copy buttons
    keep the existing label-swap behavior.
- Timeline keyboard shortcuts gained `c` (copy run id) and `y`
  (yank current event's hash). Hint row in `run.html` updated.
- Diff page payloads pick up the same syntax highlight via shared
  `highlightJSON` + `payload-json` styles.
- New `inspect.WithDBPath` option — populates the topbar context
  chip with the basename of the open DB; `cmd/starling-inspect`
  passes it automatically.

### Inspector UI follow-ups

- **Brand colors locked.** Primary `--accent` is the Go teal
  (`#00add8` dark / `#0089aa` light) used for the brand mark,
  active tab, Replay button, run-id hover, lifecycle event kinds,
  callid links, and JSON keys. Secondary `--accent-green`
  (`#14d693` / `#0e9a6c`) covers success: validated badge, live
  pill, tool-family event kinds, JSON strings.
- **Neutral grey palette.** `--gray-1..12` flipped from a slate
  (blue-cast) ramp to true R=G=B neutral. Background goes to pure
  black (`#000`); surfaces step `#0a0a0a → #141414 → #1c1c1c` for
  charcoal-grey-on-black, no blue tint.
- **Square corners.** `--radius-pill` collapsed `999px → 3px`. All
  badges, chips, status-tabs, live-pill, status pill are now
  rectangular. Brand glyph and live-pill dot are square.
- **Reload-spinner brand mark.** Replaces the colored square next
  to "starling-inspect" with the same circular-arrow SVG the
  starling-docs site uses as its favicon. Tiny rotation on hover.
- **Diff page is click-to-pick.** New server-side `diffOptions`
  surfaces all recorded runs through two `<select>` dropdowns
  (`{shortID · started · status}`), so you no longer paste run
  IDs to compare. Falls back to the original text inputs if
  `ListRuns` errors. Diverging rows now use a 3-px colored left
  rail instead of a full red/yellow background flood — the page
  reads as differences, not alarms.
- **Run detail page consolidated.** Run-head merges the live pill
  and validation badge onto one row; the duplicate "9 events"
  caption is gone (the totals strip already showed it). Totals
  use a `totals-compact` variant on this page, single-line
  inline `dt`/`dd` rather than the dashboard's stacked block.
  The "EVENT TIMELINE" / "Event detail" pane labels are dropped;
  the column structure speaks for itself. Keyboard hints are
  folded behind a "?" toggle button so the timeline isn't
  framed by a permanent four-line legend.
- **JSON pane: wrap by default.** `.payload-json` now uses
  `white-space: pre-wrap` plus `overflow-wrap: anywhere` so long
  string values reflow inside the pane width and base64 hashes
  break mid-token instead of forcing a horizontal scroll. A new
  `[data-wrap-toggle]` icon button (and the `w` keyboard
  shortcut) flips back to strict-pre for diff-by-eye work; the
  choice persists per-user via `localStorage`. Re-applied on
  every HTMX detail-pane swap.
- **More keyboard shortcuts on the run page.** `c` copies the run
  id, `y` yanks the active event's hash to the clipboard, `w`
  toggles JSON line wrap. The transient toast confirms each
  copy without clobbering the displayed text.
- Smaller polish: search inputs gain a leading magnifier icon
  with proper left-padding so the placeholder doesn't overlap;
  topbar uses solid-bg (no blur) and a square brand glyph on its
  ring; tabs render as connected boxes inside one bordered
  container with vertical separators.

## [v0.1.0-beta.1] - 2026-04-30

First public beta tag. Subsequent betas will record their deltas
relative to this baseline; the contents of the v0.1.0-beta.1 cut
itself are recoverable from `git log v0.1.0-beta.1`.

[Unreleased]: https://github.com/jerkeyray/starling/compare/v0.1.0-beta.1...HEAD
[v0.1.0-beta.1]: https://github.com/jerkeyray/starling/releases/tag/v0.1.0-beta.1
