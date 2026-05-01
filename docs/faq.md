# FAQ

Short answers to questions that come up enough to deserve a
permanent home. For the conceptual model, see
[mental-model.md](mental-model.md); for narrow API answers, the
reference pages under [reference/](reference) are the source of
truth.

## Why does `Resume` reject terminal runs?

Because the terminal event commits a Merkle root over every prior
leaf in the chain. Appending past it would invalidate the
commitment. `ErrRunAlreadyTerminal` is the runtime refusing to
silently corrupt the audit guarantee.

If you actually want to extend a different chain that *looks like*
the original, fork the log:

```go
err := eventlog.ForkSQLite(ctx, "runs.db", "branch.db", runID, 3)
```

That gives you a brand-new file truncated at `seq=3` of the named
run. Open it read-write and `Resume` extends the new chain. See
[cookbook/branching.md](cookbook/branching.md).

## Why is `merkle` a top-level package now?

Up to `v0.1.0-beta.1` it was `internal/merkle`. The 32-byte
Merkle root recorded on every terminal event is *the*
cryptographic commitment that makes the log tamper-evident; if
third parties want to write events compatible with Starling, or
verify a recorded root themselves, they need exactly the same
implementation the runtime uses. Hiding it behind `internal`
forced consumers to copy-paste; promoting it lets them just
import.

The package surface is small (`Root([][]byte) []byte`) and stable
across `event.SchemaVersion` bumps.

## How do I do session-level budgets?

You don't, directly - there's no `Session` primitive yet. The
recommended pattern: one Run per logical message, an external ID
column on your side that groups them, and a budget cap *per Run*
plus a process-level accumulator if you really need a hard ceiling
across the group.

A first-class `Session` (N Runs sharing a Budget) was triaged but
deferred from the current beta. If you hit this in practice, file
an issue describing the workflow - the design space is wide and
real usage is the best constraint.

## What does prompt caching actually do?

For Anthropic adapters specifically: when you set
`provider.Message.Annotations["cache_control"]` on a message, the
server stores that prefix and returns "cache read" tokens on
subsequent calls that hit the same prefix. Pricing applies tiered
multipliers (0.1× for hits, 1.25–2× for writes, depending on TTL).

Starling surfaces the cache token counts on
`AssistantMessageCompleted` (`CacheReadTokens` /
`CacheCreateTokens`) and aggregates them into
`RunResult.CacheStats`. The cost stamped on the event already
reflects the multipliers - you don't compute them yourself.

OpenAI's automatic prompt caching is reflected in their billing
but not separately reported to the SDK; Starling can't surface it.
Bedrock's cache flows through the same fields when the underlying
model supports it. See
[reference/cost-model.md](reference/cost-model.md).

## Can I use a different provider for replay than for the original run?

Not by default. `Replay` reads the recording's
`RunStarted.{ProviderID, APIVersion, ModelID}` and refuses unless
the live agent matches, returning
`ErrProviderModelMismatch`. The check trips before any turn
executes, so the failure mode is explicit rather than "my replay
diverges in confusing ways".

Pass `WithForceProvider()` when comparing providers is the point
(e.g. running the same recorded conversation through OpenAI and
Anthropic to compare answers). Force only bypasses the upfront
identity check; the byte-for-byte replay will still flag any
divergent event as `ErrNonDeterminism`.

## Why does my replay diverge on the first event?

Almost certainly `Config.Model`, `Provider.Info().ID`, or the
system prompt differs from the original run. Those values are
stamped into `RunStarted.payload`; replay byte-compares the
re-emitted `RunStarted` against the recording, so any drift in
config shows up as a `payload` divergence at `seq=1`.

`starling.WithForceProvider()` skips the upfront identity check
but does **not** skip the byte comparison - see the previous
question.

## Why does my tool's replay diverge mid-run?

The tool's output today differs from the recording. Common causes:

- **Time / randomness** in the tool: wrap with `step.SideEffect`
  inside the tool body so the recorded result is replayed
  literally instead of recomputed.
- **External state** (HTTP, DB row, file contents) changed since
  the recording.
- **Non-deterministic JSON marshaling** (map key ordering, omitted
  zero fields, etc.). Audit your output type.

See [reference/step-primitives.md](reference/step-primitives.md)
for the recording primitives.

## How do I stream tokens to a frontend?

Use `Agent.RunStream` for a typed channel of `AgentEvent`
(`TextDelta`, `ToolCallStarted`, `ToolCallEnded`, `Done`):

```go
runID, ch, err := agent.RunStream(ctx, goal)
for ev := range ch {
    switch v := ev.(type) {
    case starling.TextDelta:        sendToClient(v.Text)
    case starling.ToolCallStarted:  // ...
    case starling.Done:             return
    }
}
```

Per-chunk streaming (each provider chunk as it arrives) is a
future addition behind the same `AgentEvent` interface. Today
`TextDelta` carries the full assistant text on
`AssistantMessageCompleted`. The CBOR-flavored low-level
`Agent.Stream` exposes every event with the full envelope if you
want to drive your own projection.

## Why does the inspector run read-only?

The inspector binary opens its SQLite database with
`eventlog.WithReadOnly`. That's the single line that keeps the
audit property intact: even if a bug or compromised dependency
tried to mutate the log, `Append` would return `ErrReadOnly`
before reaching the table. Replay (in dual-mode binaries) writes
to a fresh in-memory log, never the source.

## What's a "dual-mode binary"?

A user binary that links both your agent and Starling's CLI
helpers. The user dispatches on `os.Args[1]`:

```go
if os.Args[1] == "inspect" { starling.InspectCommand(factory).Run(...); return }
if os.Args[1] == "replay"  { starling.ReplayCommand(factory).Run(...); return }
// ... agent run path ...
```

The factory returns a `*starling.Agent` configured equivalently to
the original run. This is the only way to get replay-from-CLI;
the stock `cmd/starling` binary doesn't link a factory.

See [reference/replay.md](reference/replay.md) for the full
shape.

## Where do I track an open question?

Open an issue at
<https://github.com/jerkeyray/starling/issues>. The triage doc
([temp_notes/REVIEWER_FEEDBACK_PLAN.md](../temp_notes/REVIEWER_FEEDBACK_PLAN.md))
lists items deferred from the current beta - feel free to upvote
or expand on any.
