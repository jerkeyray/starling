# Starling docs

Conceptual and reference material for Starling. The
[top-level README](../README.md) is the marketing/overview surface;
this directory is for users who want to actually understand what's
going on.

## Wave A — start here

- [getting-started.md](getting-started.md) — install, your first
  agent, tools, durable storage, replay.
- [mental-model.md](mental-model.md) — what a Run is, when it
  terminates, how to think about Runs vs Turns vs Resume vs new
  Runs, what replay actually checks.

## Coming next

- `cookbook/` — branching, forking, manual event writing,
  multi-turn conversations, deterministic test providers.
- `reference/` — every event Kind, every payload field, the
  step-package primitives, the cost model, tool lifecycle, replay
  internals, metrics names, and the SQLite save-file layout.
- `faq.md`.
