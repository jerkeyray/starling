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

## [v0.1.0-beta.1] - 2026-04-30

First public beta tag. Subsequent betas will record their deltas
relative to this baseline; the contents of the v0.1.0-beta.1 cut
itself are recoverable from `git log v0.1.0-beta.1`.

[Unreleased]: https://github.com/jerkeyray/starling/compare/v0.1.0-beta.1...HEAD
[v0.1.0-beta.1]: https://github.com/jerkeyray/starling/releases/tag/v0.1.0-beta.1
