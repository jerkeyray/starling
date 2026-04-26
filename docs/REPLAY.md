# Replay cookbook

Starling records every agent run as a BLAKE3-chained event log. Once
a run is persisted you can re-execute it against the same `Agent`
and get one of two outcomes:

1. The re-run emits the same events in the same order with the same
   payloads — the run is reproducible.
2. Some event diverges from the recording — Starling returns an
   error wrapping `starling.ErrNonDeterminism` pointing at the first
   difference.

This document is the user-facing guide to that second path: how to
run it, what causes drift, and how to write tools that replay
cleanly.

## Why replay?

Two reasons to care about replay:

- **Crash recovery / forensics.** The process died at turn 5. Open
  the SQLite log, replay the run, inspect the last event to see
  what was in flight. No "what was the agent thinking?" mystery.
- **Non-determinism detection.** Your tool started hitting a
  different upstream shard last Tuesday and now returns subtly
  different JSON. Replaying yesterday's runs against today's code
  catches the drift as `ErrNonDeterminism` instead of silent
  behavior change.

Because replay is byte-exact (events are canonical CBOR before the
hash chain sees them), even reordered JSON keys in a tool output
count as drift.

## The API

```go
func Replay(
	ctx context.Context,
	log eventlog.EventLog,
	runID string,
	a *Agent,
) error
```

Returns `nil` on clean replay. Returns an error wrapping
`starling.ErrNonDeterminism` on drift:

```go
if err := starling.Replay(ctx, log, runID, a); err != nil {
	if errors.Is(err, starling.ErrNonDeterminism) {
		// drift — log the error message for the first diverging seq
	}
	return err
}
```

Other errors (log read failures, tool execution failures, ctx
cancellation) surface verbatim — they aren't `ErrNonDeterminism`.

## What "same agent" means

When you call `Replay(ctx, log, runID, a)`, Starling runs the same
ReAct loop as a fresh agent, but with two fields replaced:

- `a.Provider` is swapped for a replay-driven provider that yields
  the recorded LLM output chunk-for-chunk. Your original Provider is
  ignored.
- `a.Log` is swapped for a scratch in-memory log so the re-run
  doesn't write back into the persisted log.

Everything else on the `Agent` must match the original run:

- **Tools** — the same `tool.Tool` implementations, registered by
  the same names, returning the same JSON for the same inputs.
  Adding, removing, or renaming tools between record and replay
  causes drift.
- **MCP tools** — `tool/mcp` tools are still external tools. Replay
  calls the MCP server again and compares the returned JSON with the
  recording. Keep the same server reachable, or wrap the remote effect
  behind your own deterministic tool if you need portable replay from
  a log alone.
- **Model** — `Config.Model` must match. The replay provider honors
  the name from the recording.
- **System prompt** — `Config.SystemPrompt` must match byte-for-byte.
- **Budget and MaxTurns** — don't strictly have to match (replay
  doesn't re-trip budgets), but keeping them identical makes the
  intent obvious.

## End-to-end example

Open a persisted log and replay a specific run:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider/openai"
	"github.com/jerkeyray/starling/tool"
)

func main() {
	ctx := context.Background()

	log, err := eventlog.NewSQLite("runs.db")
	if err != nil {
		panic(err)
	}
	defer log.Close()

	// Rebuild the agent exactly as it was configured at record time.
	// Provider and Log values are ignored during replay but must be
	// non-nil so Agent.validate is happy.
	prov, _ := openai.New(openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	a := &starling.Agent{
		Provider: prov,
		Tools:    []tool.Tool{ /* same tools, same names */ },
		Log:      log,
		Config: starling.Config{
			Model:    "gpt-4o-mini",
			MaxTurns: 8,
		},
	}

	runID := os.Args[1]
	switch err := starling.Replay(ctx, log, runID, a); {
	case err == nil:
		fmt.Println("replay clean")
	case errors.Is(err, starling.ErrNonDeterminism):
		fmt.Printf("drift: %v\n", err)
	default:
		fmt.Printf("replay error: %v\n", err)
	}
}
```

## Determinism rules for tools

If your tool reads the wall clock, generates randomness, or makes a
side-effectful call (HTTP, file read, database query), reach for the
`step` package's deterministic helpers. Under `ModeLive` they
execute normally and record the result; under replay they return
the recorded value and skip the effect.

| Instead of… | Use |
|---|---|
| `time.Now()` | `step.Now(ctx)` |
| `rand.Uint64()` | `step.Random(ctx)` |
| anything else that isn't pure | `step.SideEffect(ctx, name, fn)` |

`step.SideEffect` is the general case:

```go
import "github.com/jerkeyray/starling/step"

// Inside a tool's Execute:
user, err := step.SideEffect(ctx, "fetch_user", func() (User, error) {
	return httpClient.GetUser(ctx, userID)
})
```

On replay the HTTP call is skipped and `user` is decoded from the
recorded `SideEffectRecorded` event.

## Common causes of `ErrNonDeterminism`

- **Tool output changed.** Upstream returned different JSON, or the
  tool now encodes fields in a different order. Canonical CBOR
  makes key order irrelevant, but value drift is caught.
- **New tool planned mid-run.** The recording has no evidence of a
  tool call the replay tries to make.
- **Prompt edited.** `Config.SystemPrompt` or the user goal changed
  between record and replay. The `RunStarted` payload hashes won't
  match.
- **Model swapped.** `Config.Model` differs.
- **Tool reads wall clock / randomness directly.** Bypass the
  deterministic helpers and every replay drifts. Wrap such reads in
  `step.Now` / `step.Random` / `step.SideEffect`.
- **Map iteration order leaks into a tool result.** Tools must sort
  map outputs (or use CBOR's canonical encoding) so the result
  payload is deterministic.

## Power user: constructing replay manually

Most callers should use `starling.Replay`. If you need to drive the
replay machinery yourself (e.g. testing a custom tool in isolation),
`step.NewContext` accepts `Mode: ModeReplay` plus `Recorded: []event.Event`
and the rest of the `step` helpers route through the recording.
See [`step/config.go`](../step/config.go) for the full surface.
