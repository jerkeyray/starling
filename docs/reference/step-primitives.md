# Reference: deterministic primitives

`step.Now`, `step.Random`, and `step.SideEffect` are the primitives
that let your code use clocks, randomness, and external services
inside an agent run while keeping the run replayable. Source of
truth: [`step/step.go`](../../step/step.go).

The contract: every call emits a `SideEffectRecorded` event into
the chain. In live mode the recorded `Value` is the freshly
produced value; in replay mode the runtime returns the previously
recorded `Value` without re-invoking the underlying source. The
chain therefore lays out byte-identically across live + replay.

> **Where you can call them.** All three require a `step.Context`
> attached to `ctx`. Inside `Agent.Run`, `Resume`, and tool
> implementations executed by the agent, the context is already
> wired. Outside that — bare HTTP handlers, init code, helper
> goroutines — calling these panics. The cookbook's [manual-writes
> entry](../cookbook/manual-writes.md) covers writing events
> without `Agent.Run`.

## `step.Now(ctx) time.Time`

Returns the current wall-clock time, recording the value as
nanoseconds-since-epoch under the reserved name `"now"`.

```go
deadline := step.Now(ctx).Add(30 * time.Second)
```

- **Live mode**: reads from `Config.ClockFn` (defaults to
  `time.Now`), records, returns.
- **Replay mode**: pops the next recorded `SideEffectRecorded`
  expecting `Name == "now"`, decodes the int64 nanos,
  re-emits a matching event into the sink log, returns
  `time.Unix(0, nanos)`.

A run that calls `time.Now()` directly instead of `step.Now(ctx)`
will diverge during replay because the wall-clock value won't match
the recording. Provider-emitted timestamps inside model responses
are not affected — they ride through `event.Event.Timestamp` set by
the event log itself.

## `step.Random(ctx) uint64`

Cryptographically random 64-bit integer. Recorded under the
reserved name `"rand"`.

```go
attemptID := step.Random(ctx) % 1000
```

- **Live mode**: reads 8 bytes from `crypto/rand`, encodes
  big-endian, records, returns.
- **Replay mode**: pops the next `SideEffectRecorded`,
  decodes uint64, re-emits, returns.

The reserved names `"now"` and `"rand"` are taken — user
`SideEffect` calls must not use them.

## `step.SideEffect[T](ctx, name, fn) (T, error)`

The general primitive: run `fn` once, record its output, return.
On replay, skip `fn` entirely and return the recorded output.

```go
ip, err := step.SideEffect(ctx, "lookup_ip",
    func() (string, error) {
        return whois.Lookup("example.com")
    })
```

- **Live mode**: invokes `fn`, encodes the return value via
  canonical CBOR (`event.EncodePayload`), emits a
  `SideEffectRecorded{Name: name, Value: encoded}`, returns
  `(value, nil)`.
- **Replay mode**: pops the next `SideEffectRecorded` expecting
  `Name == name`, decodes into `T`, re-emits a matching event,
  returns. `fn` is **never** called in replay mode.

Constraints:

- `T` must be CBOR-serialisable. Custom types satisfy this if
  every exported field is a CBOR-supported type or has appropriate
  `cbor:"..."` tags.
- `name` is a stable string identifying the call site. Keep it
  unique within a run (an LLM tool wraps each tool call with its
  own internal name automatically; `SideEffect` is for direct use
  in agent code).
- If `fn` returns an error, the result is **not** recorded —
  replay will re-execute `fn` to reproduce the error path.

## When to use each

| Need | Primitive |
| --- | --- |
| Wall clock | `step.Now` |
| Randomness | `step.Random` |
| HTTP / filesystem / DB / any external service | `step.SideEffect` |
| Tool with idempotency, retries, schema | `tool.Tool` (the runtime wraps it) |

If your tool is already a `tool.Tool` and runs through the agent
loop, you don't need `step.SideEffect` on top — `step.CallTool`
already records `ToolCallScheduled`/`ToolCallCompleted` for you.

## Failure modes

All three panic on:

- Calling without a `step.Context` on `ctx` (programmer error;
  the agent loop always provides one).
- The event log rejecting the `SideEffectRecorded` write — once
  this happens the run's hash chain is incomplete and any further
  work would produce a non-replayable trace, so the runtime fails
  loudly rather than silently corrupt.
- Replay encountering an event that doesn't match the expected
  `Name` or kind. Wrapped as `step.ErrReplayMismatch`; surfaced by
  the replay package as `ErrNonDeterminism`.

## See also

- [reference/events.md](events.md) — `SideEffectRecorded` payload.
- [reference/replay.md](replay.md) — how recorded side effects are
  consumed during replay.
- [`step/config.go`](../../step/config.go) for `ClockFn` and the
  other live-mode knobs.
