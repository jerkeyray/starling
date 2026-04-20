# Resume — crash-and-continue

`(*Agent).Resume` picks up an in-progress run that was interrupted
mid-flight — typically because the process hosting it died. It reads
the events already in the log, rebuilds the conversation state, and
re-enters the agent loop so the run can finish in the new process.

Resume is the operational counterpart to Replay. Replay re-executes a
run for determinism verification and produces no new events; Resume
writes fresh events onto the existing hash chain so the log ends with
a terminal event exactly as if the crash had never happened.

## When to use Resume

- The box running an agent crashed, restarted, or was re-scheduled by
  an orchestrator. The run's events are durable in your `EventLog`
  (SQLite on disk, Postgres, etc.) but the run itself never terminated.
- A long-running run needs to span a deployment: drain one process,
  point the next one at the same log + runID.
- You want to inject a follow-up user message into an already-running
  conversation without starting a new run. Pass `extraMessage`.

Resume is **not** a retry policy for failed runs. A run that ended
with `RunFailed` / `RunCancelled` / `RunCompleted` is terminal; Resume
returns `ErrRunAlreadyTerminal` and refuses to touch it.

## API

```go
res, err := agent.Resume(ctx, runID, "")                       // common case
res, err := agent.ResumeWith(ctx, runID, "", opts...)         // with options
res, err := agent.Resume(ctx, runID, "please try the other API") // inject a user message
```

Both calls return a `RunResult` whose `Events` field covers the full
merged history — everything from the original process plus everything
the resuming process appended.

Error sentinels (in `errors.go`):
- `ErrRunNotFound` — no events for `runID`.
- `ErrRunAlreadyTerminal` — last event is terminal.
- `ErrSchemaVersionMismatch` — `RunStarted` was written under a
  schema version this binary does not understand.
- `ErrPartialToolCall` — run has unpaired `ToolCallScheduled` events
  and `WithReissueTools(false)` was passed.
- `ErrRunInUse` — another writer advanced the chain between our tail
  read and our first append. Two processes racing to resume the same
  run; the loser bails.

## Partial-turn semantics

The original process can die at any point between two events. Resume
classifies the tail of the chain and picks the right re-entry:

| Last event recorded           | Resume does                                                      |
|-------------------------------|------------------------------------------------------------------|
| `RunStarted` only             | Enter the loop at turn 0 with the original goal.                 |
| `TurnStarted` (no completion) | Drop the incomplete turn; re-issue `LLMCall` at the same turn.   |
| `AssistantMessageCompleted`   | Clean turn boundary; continue with the next turn.                |
| `ToolCallScheduled` (no pair) | Re-issue the tool call (or refuse — see below).                  |
| `ToolCallCompleted`/`Failed`  | Re-issue any sibling calls from the same turn that never paired. |

### Re-issuing pending tool calls

When the last recorded event is `ToolCallScheduled` without a matching
`Completed`/`Failed`, Resume's default is to **re-run** the tool call
with a fresh `CallID`. That's the right choice for the common case —
a network fetch that got cut off, a read-only lookup, an idempotent
API call — and matches what a retry-minded user would do manually.

Some tools mutate external state: sending email, charging a card,
writing to a shared filesystem. For those, silently re-running is
worse than failing loudly. Pass `WithReissueTools(false)`:

```go
res, err := agent.ResumeWith(ctx, runID, "", starling.WithReissueTools(false))
// returns ErrPartialToolCall if any tool call is mid-flight
```

The log is not mutated in the refuse case: no `RunResumed` event is
emitted. The run remains in the same state it was in before Resume
was called, so a later inspector or manual operator can still see the
orphaned `ToolCallScheduled` as the chain's tail.

Re-issued tool calls get fresh `CallID`s. The chain records what
actually ran, not what was originally attempted — consistent with
Starling's general "events are ground truth" posture. The
`RunResumed.PendingCalls` field carries the orphan count so the
inspector can visually tie orphaned `ToolCallScheduled` entries back
to their reissued replacements.

Reissued calls inherit the `TurnID` of the pre-crash turn they belong
to; no fresh `TurnStarted` precedes them. In the inspector a
`RunResumed` divider will sit between the original orphan and its
reissue, with both sharing the same `TurnID`.

## The `RunResumed` event

Every Resume emits a `RunResumed` event before anything else. It
carries:

- `AtSeq` — the sequence number of the last event recorded before
  this resume (the one whose hash seeds the next event's `PrevHash`).
- `ExtraMessage` — the user-injected message, if any. If non-empty
  this event is immediately followed by `UserMessageAppended`.
- `ReissueTools` — whether `WithReissueTools(true)` was in effect.
- `PendingCalls` — number of unpaired `ToolCallScheduled` events at
  the resume point.

`RunResumed` is non-terminal. It does not count toward any budget or
turn-count cap; it's a seam marker that makes the log legible.

## Budget semantics on Resume

- `MaxWallClock` **resets** on every Resume. It's a per-process cap;
  persisting it would silently penalize every future resume of a
  long-lived run.
- `MaxTurns` counts **total** turns across the run. If the original
  process already consumed `MaxTurns - 1` turns, Resume will hit the
  cap on its first turn and return `ErrMaxTurnsExceeded`.
- `MaxTokens` / `MaxCostUSD` reset at the process boundary for
  **inline enforcement** — the step-level counters start at zero in
  the resumed process, so a resumed run can consume up to one
  process's worth of budget before tripping. The `RunResult.Stats`
  returned by Resume *is* cumulative (it aggregates every event in
  the merged log), and terminal-event payloads reflect the full run,
  so post-hoc accounting is still correct. If you need a hard
  cumulative cap, subtract the pre-resume usage from the budget
  before calling Resume.

## Namespace check

If the Agent has a `Namespace`, Resume validates that `runID` begins
with `<Namespace>/`. A namespace mismatch is a configuration error
and fails before any log read.

## What Resume does not do

- It does not re-run side effects. Deterministic values already
  recorded as `SideEffectRecorded` stay in the log; Resume does not
  replay them. The resumed portion uses live clock / RNG / side
  effects as normal.
- It does not "roll back" events. The original chain is inviolate —
  everything after the resume seam is new growth, not a rewrite of
  history.
- It does not coordinate between writers. If two processes try to
  Resume the same run concurrently, the first `Append` wins; the
  second sees `ErrRunInUse` and must decide whether to retry.
- It does not validate the pre-resume portion of the chain. Run
  `eventlog.Validate` if you want that guarantee; Resume accepts
  whatever the log returns.

## Example

```go
// First process: starts a run, then crashes somewhere in turn 3.
goal := "summarize yesterday's tickets"
agent := &starling.Agent{Provider: p, Log: db, Config: cfg}
res, err := agent.Run(ctx, goal)
// ... process dies ...

// Second process, same DB:
agent := &starling.Agent{Provider: p, Log: db, Config: cfg}
res, err := agent.Resume(ctx, previousRunID, "")
// res.TerminalKind == RunCompleted (or whatever the resumed portion ends with)
// res.Events carries the full merged history
```
