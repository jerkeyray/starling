# Debugging a failed run with replay

Your agent failed in production. The user says "it worked yesterday."
You have a SQLite event log and ten minutes before standup.

This is the workflow Starling is built for.

## The pitch

Every run Starling executes is recorded to an append-only,
BLAKE3-chained event log: every prompt, every model response, every
tool call and its arguments and output, every usage tick, every
budget decision, in order, with hashes. That log is enough to
**re-execute the run byte-for-byte against your current agent
code** — same provider, same tools, same config — and see the exact
step where today's behaviour diverges from the recording.

No distributed tracing UI to piece together. No "let me add some
prints and redeploy." The failing run, the passing run, and the
diff between them, in one local binary.

## The 3-minute walkthrough

### 1. Wire dual-mode into your agent binary

One place to configure your agent, used by both the run path and
the inspector's replay path. The inspector calls back into the
same factory that built the original run — that's the whole reason
replay stays faithful.

```go
package main

import (
    "context"
    "log"
    "os"

    starling "github.com/jerkeyray/starling"
    "github.com/jerkeyray/starling/eventlog"
    "github.com/jerkeyray/starling/provider/openai"
    "github.com/jerkeyray/starling/replay"
    "github.com/jerkeyray/starling/tool"
)

func buildAgent(_ context.Context) (*starling.Agent, error) {
    prov, err := openai.New(openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
    if err != nil {
        return nil, err
    }
    logStore, err := eventlog.NewSQLite("./runs.db")
    if err != nil {
        return nil, err
    }
    return &starling.Agent{
        Provider: prov,
        Tools:    []tool.Tool{ /* your tools */ },
        Log:      logStore,
        Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 8},
    }, nil
}

func main() {
    if len(os.Args) > 1 && os.Args[1] == "inspect" {
        factory := replay.Factory(func(ctx context.Context) (replay.Agent, error) {
            return buildAgent(ctx)
        })
        cmd := starling.InspectCommand(factory)
        if err := cmd.Run(os.Args[2:]); err != nil {
            log.Fatal(err)
        }
        return
    }
    // ... your normal agent run ...
}
```

A complete runnable version is in
[`examples/m1_hello`](../examples/m1_hello).

### 2. Open the event log

```sh
./your-agent inspect ./runs.db
```

The inspector binds to loopback on a free port, opens the runs list
in your browser, and prints the URL. Sort by status, find the
`failed` or `cancelled` run you need to debug. Click it.

The event timeline shows every step in order: model calls, tool
calls and their arguments, budget decisions, usage ticks, terminal
events. The header shows a hash-chain validation badge — if the log
was tampered with, you'll see a red ✗ before you waste time on a
false lead.

### 3. Click Replay

The inspector spins up a fresh agent via your factory, re-executes
the recorded run step-by-step, and streams both the recorded events
and the freshly-produced events into a side-by-side view.

Rows in lock-step show green. The first row that diverges shows
red, with a one-line reason: `tool=fetch call=c1 recorded output
hash a4b2… got 9e01…`, or `model output differs at turn 2`, or
`tool call missing at seq 12`.

That's the step you need to look at. Not "somewhere in this
200-event run"; the exact seq.

## What replay catches

- **Tool output changed.** External API started returning different
  JSON, or the tool's own logic changed. The diff shows the old and
  new output hashes at the diverging call.
- **Model output changed.** You upgraded the model, switched
  providers, or the provider silently rolled a fine-tune. The diff
  shows the diverging assistant turn.
- **Prompt changed.** You tweaked your system prompt and forgot
  what it used to be. The diff shows the first turn where inputs
  no longer match.
- **Non-deterministic code crept in.** A tool called `time.Now()`
  directly instead of `step.Now()`, or used a raw random source.
  Replay pins non-determinism to the exact offending step.
- **A dependency your tool calls changed behaviour.** Library
  upgrade, config flag flip, database migration. Replay doesn't
  care *why* the output differs, only *where*.

## What replay won't catch

- **Transient failures that won't recur.** If the failure was a
  network blip and the current run succeeds, replay shows a
  diverging step — your job is to interpret it as "flake, not
  regression."
- **Non-determinism you haven't routed through Starling.** Calls to
  `time.Now()`, `rand.Read`, or live HTTP directly from a tool will
  appear as divergence on every replay, not just the one you're
  debugging. Use `step.Now`, `step.Random`, and `step.SideEffect`
  from tools. See [`docs/REPLAY.md`](./REPLAY.md) for the full
  determinism rules.

## Scripting it

Replay also works from Go — useful in CI, or when you want to diff
a run against a candidate agent config before merging:

```go
if err := starling.Replay(ctx, log, runID, candidate); err != nil {
    if errors.Is(err, starling.ErrNonDeterminism) {
        // Candidate diverged from recording.
    }
    // ...
}
```

The inspector's Replay button is this call with a UI on top.

## See also

- [`docs/INSPECT.md`](./INSPECT.md) — inspector install, flags,
  security model.
- [`docs/REPLAY.md`](./REPLAY.md) — replay determinism rules and
  the full API.
- [`examples/m1_hello`](../examples/m1_hello) — dual-mode skeleton
  to copy from.
