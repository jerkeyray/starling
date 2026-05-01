# Multi-turn conversations

How to build a chat-style workflow on top of Starling. The short
version: **one Run per user message**, not one Run for the whole
conversation. This page explains why.

## The recommended pattern: one Run per user message

```go
for _, msg := range userMessages {
    goal := msg
    if priorReply != "" {
        goal = "Prior reply: " + priorReply + "\n\nNew question: " + msg
    }
    res, err := agent.Run(ctx, goal)
    // ...
    priorReply = res.FinalText
}
```

What this gives you:

- Every message is its own RunID, with its own Merkle root and its
  own replay boundary.
- The inspector lists each turn as a separate row, with totals you
  can read at a glance.
- `Replay(ctx, log, runID, agent)` re-executes one message in
  isolation — much faster than re-running the whole conversation to
  reach the failure.
- Budgets (cost, tokens, wall-clock) cap *each* message
  independently. A long conversation never accidentally bumps into a
  cumulative cap.
- Resume on a crashed message only re-runs that message.

If you need the model to "remember" prior messages, prepend the
relevant context (last reply, a running summary, retrieved memory)
into the goal text. The agent doesn't know or care that this is the
N-th turn of a chat — every Run looks the same to it.

## Why not one Run for the whole conversation?

Because `Agent.Run` always terminates. Its terminal event commits a
Merkle root over every prior leaf in the chain — appending past it
would invalidate the commitment. The runtime refuses
(`ErrRunAlreadyTerminal`).

`Resume` exists for *crash recovery* on a non-terminal chain — a
process died before writing the terminal event, and a successor
process picks up where the first left off. It is not the chat
continuation primitive.

If you genuinely need a single chain (e.g. for a per-conversation
audit ID that maps to a single record), use one Run per message and
correlate them via `Config.Namespace` or your own ID column outside
Starling.

## Working example

A runnable example lives at
[`examples/multi_turn/main.go`](../../examples/multi_turn/main.go).

```bash
OPENAI_API_KEY=sk-... go run ./examples/multi_turn
```

It runs three user messages through one in-memory log, threading the
prior assistant reply into each new goal. The final output lists how
many independent runs the log holds (three).

## See also

- [Mental model — When to use one Run vs many](../mental-model.md#when-to-use-one-run-vs-many).
- [Mental model — Resume vs new Run](../mental-model.md#resume-vs-new-run)
  for the crash-recovery case.
