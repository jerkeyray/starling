# Branching a recorded run

Use `eventlog.ForkSQLite` to clone a SQLite event log at a chosen
sequence boundary, producing a "branch point" you can extend without
touching the original recording.

## Why

Two common cases:

- **Counterfactual replay.** "What would the agent have done if turn
  3 had returned a different tool result?" Fork at the boundary, fix
  the recorded payload (or change tool wiring), then `Resume` from the
  same chain.
- **Audit-safe experimentation.** The original log stays intact —
  it's the canonical recording. Branches are throwaway.

## The WAL footgun

SQLite in WAL mode writes recently-committed data to `runs.db-wal`
and `runs.db-shm` *before* checkpoint. A naïve `cp runs.db
branch.db` misses any in-WAL writes — the destination is corrupt.

`eventlog.ForkSQLite` uses [`VACUUM INTO`][vacuum-into] under the
hood, which is the only SQLite-supported safe-clone. It serializes
against the writer and emits one consistent file with no sidecars.

[vacuum-into]: https://www.sqlite.org/lang_vacuum.html#vacuuminto

## Usage

```go
import "github.com/jerkeyray/starling/eventlog"

err := eventlog.ForkSQLite(ctx, "runs.db", "branch.db", runID, 3)
```

`beforeSeq` semantics:

| `beforeSeq` | Result |
| --- | --- |
| `0` | Keep every event for `runID` (bit-for-bit fork). |
| `1` | Keep nothing (the destination log has no events for `runID`). |
| `K` | Keep `seq < K` (drops `seq >= K` for `runID` only; other runs in the source log are copied verbatim). |

Errors:

- `ErrForkNotFound` — the source log has no events for `runID`.
- "dstPath already exists" — `VACUUM INTO` refuses to overwrite. The
  function checks up-front and returns a clear message.
- On any mid-operation failure the partial destination file plus its
  sidecars are removed before returning.

## Working example

A runnable example lives at
[`examples/branching/main.go`](../../examples/branching/main.go).

```bash
OPENAI_API_KEY=sk-... go run ./examples/branching
```

It records a run into `runs.db`, forks at `seq=3` into `branch.db`,
then reads back to confirm only the first two events were kept.

## Resuming the branch

To extend a forked log, open the destination read-write and call
`Agent.Resume` on a fresh agent:

```go
dst, _ := eventlog.NewSQLite("branch.db")
agent := &starling.Agent{
    Provider: prov,
    Log:      dst,
    Config:   originalConfig,
}
_, err := agent.Resume(ctx, runID, "")
```

Resume re-reads the truncated chain, replays the conversation
history into the model, and starts a new turn (with `RunResumed` as
the marker event). The Merkle root recorded on the eventual terminal
event commits to *this* branch's leaves, distinct from the original.

## See also

- [Mental model — Resume vs new Run](../mental-model.md#resume-vs-new-run)
  for when forking is the right call versus starting fresh.
- [`eventlog/fork.go`](../../eventlog/fork.go) for the implementation.
