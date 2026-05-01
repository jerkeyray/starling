# Starling docs

Conceptual and reference material for Starling. The
[top-level README](../README.md) is the marketing/overview surface;
this directory is for users who want to actually understand what's
going on.

## Wave A - start here

- [getting-started.md](getting-started.md) - install, your first
  agent, tools, durable storage, replay.
- [mental-model.md](mental-model.md) - what a Run is, when it
  terminates, how to think about Runs vs Turns vs Resume vs new
  Runs, what replay actually checks.

## Cookbook

- [cookbook/branching.md](cookbook/branching.md) - WAL-safe SQLite
  fork via `eventlog.ForkSQLite`, with a runnable example.
- [cookbook/manual-writes.md](cookbook/manual-writes.md) - write
  events directly without `Agent.Run`, using the public `merkle`
  package for the terminal root.
- [cookbook/multi-turn.md](cookbook/multi-turn.md) - chat-style
  workflows: one Run per user message, prior reply threaded as
  context.

## Reference

- [reference/events.md](reference/events.md) - every event `Kind`,
  payload shape, and the `Annotations` keys providers read.
- [reference/step-primitives.md](reference/step-primitives.md) -
  `step.Now`, `step.Random`, `step.SideEffect`.
- [reference/cost-model.md](reference/cost-model.md) - built-in
  pricing, cache multipliers, `budget.RegisterPricing`.
- [reference/tools.md](reference/tools.md) - `Tool` interface,
  `tool.Typed`, `tool.Wrap` middleware, retries, replay rules.
- [reference/replay.md](reference/replay.md) - what `Replay`
  compares, divergence classes, dual-mode binaries.
- [reference/metrics.md](reference/metrics.md) - every Prometheus
  name and OpenTelemetry span emitted.
- [reference/save-file.md](reference/save-file.md) - SQLite
  schema, WAL footgun, CBOR payload layout.

## FAQ

- [faq.md](faq.md) - quick answers to the recurring questions.

## Coming next

- More cookbook entries: deterministic test providers (using
  `starlingtest`), MCP tools.
