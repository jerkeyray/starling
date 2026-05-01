# Reference: replay

How replay re-executes a recorded run, what it actually compares,
how divergence surfaces, and how to wire it into your own
binaries. Source of truth: [`replay/`](../../replay) and
[`replay_api.go`](../../replay_api.go).

## What replay does

Given a recorded run and an `Agent` configured equivalently:

1. Read the run's events from the source log.
2. Refuse if the agent's
   `Provider.ID`/`APIVersion`/`Config.Model` disagree with the
   recording's `RunStarted` (override with `WithForceProvider`).
3. Build a **replay-mode provider** from the recorded assistant
   messages — no live model calls.
4. Re-execute the agent loop with that provider, the original
   tools, and a **fresh in-memory sink log**.
5. Compare each freshly-emitted event against the recording. The
   first byte-level mismatch closes the run with `RunFailed` and
   bubbles up as `ErrNonDeterminism`.

The caller's source log is **not** mutated. Replay is read-only on
the audit surface.

## Public API

```go
import starling "github.com/jerkeyray/starling"

err := starling.Replay(ctx, log, runID, agent)
err := starling.Replay(ctx, log, runID, agent, starling.WithForceProvider())
```

Returned errors:

- `nil` — replay matched the recording.
- `errors.Is(err, starling.ErrNonDeterminism)` — divergence
  detected; an `errors.As` into `*replay.Divergence` exposes
  `Seq`, `Kind`, `ExpectedKind`, `Class`, `Reason`.
- `errors.Is(err, starling.ErrProviderModelMismatch)` — agent's
  provider/model identity disagrees with the recording. Pass
  `WithForceProvider()` to bypass.
- Tool errors propagate verbatim. A tool that returns an error in
  replay produces the same chain entry as the recording would have
  (so the divergence check remains meaningful).

The CLI counterpart is `starling replay [--force] <db> <runID>` —
documented under [`cmd/starling/main.go`](../../cmd/starling/main.go)
and only useful for dual-mode binaries that link an agent factory.

## What gets compared

Replay byte-compares the canonical CBOR of each re-emitted event
against the recording. That covers:

- The `Kind` enum.
- The `Payload` field (every per-kind struct verbatim, modulo the
  `Timestamp` which replay copies from the recording so chain
  hashes stay aligned).
- The chain hash sequence — `Seq` and `PrevHash` are computed live
  from the freshly-emitted bytes, so they match iff the bytes do.

Tools are **re-executed live**. Their output is then byte-compared
against the recorded `ToolCallCompleted.Result`. If your tool's
output depends on time, randomness, or external state, wrap the
nondeterminism in [`step.SideEffect`](step-primitives.md);
otherwise replay will surface the difference as divergence.

## Divergence classes

`replay.Divergence.Class` carries one of `step.MismatchClass`
values:

| Class | Meaning |
| --- | --- |
| `kind` | Re-emitted event kind != recorded kind at the same seq. |
| `payload` | Same kind, different bytes. The most common class. |
| `turn_id` | The fresh agent assigned a different `TurnID` to the same logical turn. Indicates a non-deterministic ID mint. |
| `exhausted` | The replay stream ran out before the recording did. Usually a clipped recording or a tool that bailed earlier than before. |

The same fields are surfaced by the inspector's replay view (the
toast on divergence and the Divergence dialog) and the SSE
`Divergence` payload exposed by `replay.Stream`.

## Streaming replay (`replay.Stream`)

Programmatic timeline for live UIs. Use this if you need to render
each `ReplayStep` as it lands rather than wait for a final
verdict.

```go
ch, err := replay.Stream(ctx, factory, log, runID)
for step := range ch {
    if step.Diverged {
        log.Printf("divergence: %s", step.DivergenceReason)
    }
    // step.Recorded and step.Produced both populated for matches.
}
```

`factory` is a `func(ctx) (replay.Agent, error)` that builds a
fresh agent per call. The inspector's `/run/{id}/replay` endpoint
is the reference consumer.

## Building a dual-mode binary

The inspector and the `starling replay` CLI are wired off your
agent's `*starling.Agent`. Bundle a factory and dispatch on
`os.Args[1]`:

```go
func main() {
    if len(os.Args) > 1 && os.Args[1] == "inspect" {
        cmd := starling.InspectCommand(myFactory)
        if err := cmd.Run(os.Args[2:]); err != nil { log.Fatal(err) }
        return
    }
    if len(os.Args) > 1 && os.Args[1] == "replay" {
        cmd := starling.ReplayCommand(myFactory)
        if err := cmd.Run(os.Args[2:]); err != nil { log.Fatal(err) }
        return
    }
    // ... normal agent run ...
}

func myFactory(ctx context.Context) (replay.Agent, error) {
    prov, err := openai.New(openai.WithAPIKey(...))
    if err != nil { return nil, err }
    return &starling.Agent{
        Provider: prov,
        Tools:    myTools,
        Config:   originalConfig,
    }, nil
}
```

The factory **must** rebuild the agent equivalently to the
original run — same tool registry, same `Config.Model`, same
provider info. The provider/model identity check rejects most
misconfigurations before any work runs; tool registry drift is
caught when the first tool call replays.

## Stock `cmd/starling`

The pre-built `cmd/starling` binary cannot replay (it has no
factory linked). It surfaces this with a clean error:

```
$ starling replay runs.db <runID>
starling: replay requires a dual-mode binary; see starling.ReplayCommand godoc
```

Build a dual-mode wrapper as above to enable replay from the CLI.

## Tests as replay regressions

`starlingtest.AssertReplayMatches(t, log, runID, agent)` is a
one-line wrapper for regression tests: record once, then replay
the same log against the same agent factory in CI. Drift in tool
behavior, provider adapters, or the agent loop surfaces as a
failed test rather than a production divergence.

## See also

- [reference/events.md](events.md) — what's actually compared.
- [reference/step-primitives.md](step-primitives.md) — how to
  wrap tool nondeterminism so replay stays clean.
- [reference/tools.md](tools.md) — tool-side replay
  expectations.
- [`replay/replay.go`](../../replay/replay.go) — the public
  package surface.
