# Writing events without `Agent.Run`

Sometimes you want Starling's audit log, inspector, and validation
without the full agent loop on top - say, an ETL job or a custom
control plane that already does its own orchestration. The chain
invariants are public; you can write events directly.

## When this is the right call

- You want a deterministically replayable record of *non-LLM* work
  that should still show up in the inspector alongside agent runs.
- You're writing a custom backend producer (not an `EventLog`
  implementation, but a writer feeding one).
- You need a side log of administrative actions (deploys, manual
  approvals) that audit consumers should be able to verify with the
  same `eventlog.Validate` they use everywhere else.

If you find yourself reimplementing `Agent.Run` step by step, stop
and use the agent loop. This pattern is for the cases above.

## Chain invariants you must respect

For every appended event:

1. The first event has `Seq == 1` and `PrevHash == nil`.
2. Each subsequent event has `Seq == prev.Seq + 1` and
   `PrevHash == event.Hash(event.Marshal(prev))`. The marshaling is
   canonical CBOR (RFC 8949 §4.2); it has to go through
   `event.Marshal` for byte-stable hashing.
3. Payloads encode through `event.EncodePayload[T]` so the per-kind
   schema bumps stay coherent.
4. The terminal event records a Merkle root over every prior leaf's
   hash, computed via the public
   [`merkle`](../../merkle/merkle.go) package - the same one the
   runtime uses, so consumers can recompute and verify.

Skip any of these and `eventlog.Validate` will report the break.

## Working example

A runnable example lives at
[`examples/manual_writes/main.go`](../../examples/manual_writes/main.go).
It writes a `RunStarted`, three `SideEffectRecorded` events, and a
terminal `RunCompleted` with a Merkle root over every leaf, then
calls `eventlog.Validate` to confirm.

```bash
go run ./examples/manual_writes
# wrote 5 events into manual.db, chain valid, merkle=837c0ea3be880423
```

Inspect with:

```bash
starling-inspect manual.db
```

The inspector treats the result identically to an agent-produced
log: timeline, validation badge, totals header, all of it.

## A word on `SideEffectRecorded.Value`

The `Value` field is `cborenc.RawMessage` - i.e. canonical CBOR
bytes, not arbitrary JSON. Encode through
[`fxamacker/cbor`](https://pkg.go.dev/github.com/fxamacker/cbor/v2)
or `cborenc.Marshal` (the runtime's canonical encoder); pasting
JSON in directly produces a corrupt event that fails to decode.

This is the same constraint the runtime is under: the chain's
byte-for-byte determinism only works if every payload is canonical.

## See also

- [`merkle`](../../merkle/merkle.go) - the public Merkle helpers.
- [`eventlog.Validate`](../../eventlog/validate.go) - chain
  validation.
- [`event/types.go`](../../event/types.go) - the full per-kind
  payload set you can pick from.
