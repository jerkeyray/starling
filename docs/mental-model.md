# Mental model

Three concepts to internalize before writing non-trivial agents:
**Run**, **Turn**, and **Event log**. Most confusion in early use of
Starling traces back to one of these. The core API surface is small
once you have the picture.

## What is a Run?

A Run is one invocation of `Agent.Run(ctx, goal)`. It owns:

- a unique ULID-style **RunID** (or `Namespace + "/" + ULID` if
  `Config.Namespace` is set)
- an **append-only event log** entry per meaningful action — every
  prompt, every tool call, every usage update, every budget decision
- a single **terminal event** that closes the chain: `RunCompleted`,
  `RunFailed`, or `RunCancelled`

A Run is "done" when (and only when) the terminal event lands. From
that point the log is immutable: the terminal event records a Merkle
root over every prior event's hash, and appending past it would
invalidate the commitment. `Resume` exists for runs that crashed
*before* the terminal event was written; once a Run terminates, it's
read-only forever.

### Lifecycle of a single Run

```
RunStarted
  └─ TurnStarted
      ├─ AssistantMessageCompleted   ← model response (with usage)
      ├─ ToolCallScheduled           ← model wanted a tool
      └─ ToolCallCompleted | ToolCallFailed
  └─ TurnStarted                     ← next turn (after a tool result, or model's choice)
      └─ AssistantMessageCompleted
  ...
  └─ RunCompleted | RunFailed | RunCancelled
```

The loop terminates when the model returns a turn with no tool calls
(success), or a budget cap trips (`RunFailed{ErrorType:"budget"}`),
or `Config.MaxTurns` is reached
(`RunFailed{ErrorType:"max_turns"}`), or `ctx` is cancelled
(`RunCancelled`).

### Tools inside a Turn

Tool calls don't get their own Turn — they're *inside* the assistant's
turn. The model emits `ToolCallScheduled`, the runtime executes the
tool, emits `ToolCallCompleted` (or `ToolCallFailed` if the tool
returned an error), and the loop kicks off a new `TurnStarted` for
the model to react to the result.

Retries and panics are part of the same call's events: a tool that
returns `tool.ErrTransient` produces multiple `ToolCallScheduled`
events with incrementing `Attempt` until it succeeds or the cap is
hit. Replay replays exactly the same retry sequence — that's what
"deterministic replay" means at the tool layer.

## When to use one Run vs many

This is the question we get most. The short version:

- **One Run = one self-contained task.** Anything you'd want to look
  at as a single unit in the inspector, replay as one operation, or
  budget against one cost limit, is one Run.
- **Multiple turns inside a Run = the model needs to think in
  several steps to finish that task.** Tool calls, reasoning,
  reading a file then editing it.
- **A new Run = a new task.** Different goal, different log entry,
  different replay boundary, different budget.

Concretely:

| Scenario | Pattern |
| --- | --- |
| User asks one question. The agent thinks, calls a tool, answers. | **One Run.** Many turns inside it. |
| Multi-turn chat: every user message is the start of new work. | **One Run per user message.** Use `Resume(ctx, prevRunID, userMsg)` if you need conversation continuity inside a single chain; use a new `Run` if each message is independent. |
| Long-running ETL: 1,000 tickets, one prompt each. | **One Run per ticket.** Cheap, lets you replay or inspect any single failure without dragging the others. |
| One Run that loops through 1,000 tickets internally. | **Probably wrong.** Replay is all-or-nothing; a single mid-run divergence forces you to re-execute the whole batch. Budgets and timelines also get unwieldy. |

Rule of thumb: if you find yourself asking "should I split this into
multiple Runs?" the answer is almost always yes.

## Resume vs new Run

`Agent.Resume(ctx, runID, extraMessage)` continues a Run that has
*not* terminated yet. It re-reads the existing events, rebuilds the
conversation history exactly as the prior process saw it, optionally
appends `extraMessage` as a `UserMessageAppended`, and runs more
turns into the same chain.

Use Resume when:

- the prior process crashed mid-run and there's no terminal event
  yet (the canonical case)
- you genuinely want the model to see a continuous chain of prior
  turns (a chat where context is the whole conversation)

Use a fresh Run when:

- the prior task is finished (the prior Run has a terminal event —
  Resume will return `ErrRunAlreadyTerminal`)
- you don't need the prior history; you want a clean slate (cheaper
  prompts, easier to reason about)

Resume preserves the budget, the tool registry hash, the schema
version. Cross-binary resume of a run started under a different
Starling version is allowed only when the schema version matches.

## What is the event log?

The log is **the source of truth**. Everything `RunResult` reports —
final text, costs, token totals, terminal kind, Merkle root — is
re-derivable by reading the events. `RunResult` is a convenience
shaped from the same data.

Three properties matter:

1. **Append-only and hash-chained.** Each event records the BLAKE3
   hash of the prior event's canonical CBOR encoding. Tampering with
   any event invalidates the chain from that point forward;
   `eventlog.Validate` walks the chain and reports the first break.
2. **Canonically encoded.** Events go through `internal/cborenc`
   (RFC 8949 §4.2 deterministic CBOR), so byte-for-byte equality is
   meaningful — that's how replay catches divergence at all.
3. **Backend-agnostic.** Memory, SQLite, and Postgres all satisfy
   the same `eventlog.EventLog` contract. The runtime never depends
   on backend-specific behavior beyond what the interface guarantees.

The Merkle root in the terminal event is the cryptographic
commitment over the run's leaves. Computing it requires the public
[`merkle`](../merkle/merkle.go) package, which is also what
third-party event producers should use to stay compatible.

## What replay actually checks

`Replay(ctx, log, runID, agent)`:

1. Reads the recording from `log`.
2. Builds a replay-mode provider from the recorded assistant
   messages — no live model calls.
3. Re-executes the agent loop, byte-comparing each re-emitted event
   against the recording.
4. Returns `nil` on a clean replay, or
   `errors.Is(err, ErrNonDeterminism)` on the first divergence.

Tools are re-executed live — that's by design. If your tool's output
depends on time, randomness, or external state, the divergence is
real signal: tool determinism is your responsibility, and Starling
gives you the primitives to lock it down (see
[`step.SideEffect`, `step.Now`, `step.Random`](../step/step.go)).

A divergence isn't a panic — it's a structured `Divergence` value
with the seq, kind, class, and reason. The inspector's replay view
shows it inline; consumers can `errors.As(err, &replay.Divergence{})`
to inspect it programmatically.

## Common confusions

> "Is `Stream` a separate primitive from `Run`?"

No. `Agent.Stream(ctx, goal)` is `Agent.Run` plus a typed channel of
projections of the same events. The Run is committed to the log
either way.

> "Can I write events without `Agent.Run`?"

Yes — that's the manual-writer pattern. As long as you respect the
chain invariants (Seq, PrevHash, canonical CBOR payloads) and use
the [`merkle`](../merkle/merkle.go) helpers for the terminal root,
the runtime, the validator, and the inspector treat your log
identically. A cookbook entry covers this in detail.

> "Does Replay use my real provider?"

No. Replay swaps your `Provider` for an internal replay provider
that serves recorded streams. Your `Tools` *are* re-run live;
configure them to be deterministic (or wrap nondeterminism in
`step.SideEffect`) for clean replays.

> "What does `SchemaVersion` mean?"

It's the format version of the events on disk. Resume and replay
both refuse runs whose `SchemaVersion` is unknown. See the
"Event log schema" section of the [README](../README.md) for the
contract. In short: a minor bump must remain resume-compatible; a
major bump means run `starling migrate <db>` first.
