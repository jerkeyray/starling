// Command m4_inspector_demo seeds a SQLite event log with a handful of
// synthetic runs so a developer can boot starling-inspect and look at
// the UI without an LLM provider key, without internet, and without
// running a real agent.
//
// Usage:
//
//	go run ./examples/m4_inspector_demo /tmp/demo.db
//	go run ./cmd/starling-inspect       /tmp/demo.db
//
// Or, with the Makefile target:
//
//	make demo-inspect
//
// The runs include one fully-valid completed run (with a real Merkle
// root, so the inspector's validation badge shows green), one failed
// run, one cancelled run, and one in-progress run — enough to exercise
// every status badge and event family the timeline color-codes.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/merkle"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: m4_inspector_demo <db-path>")
		os.Exit(2)
	}
	path := os.Args[1]

	// Wipe any prior demo so re-running is idempotent. We deliberately
	// only wipe SQLite sidecars — never an arbitrary path — to keep
	// fat-fingers from blowing away the user's real data.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suffix)
	}

	log, err := eventlog.NewSQLite(path)
	if err != nil {
		die("open log: %v", err)
	}
	defer log.Close()
	ctx := context.Background()

	now := time.Now()
	seedHappyRun(ctx, log, "demo-completed", now.Add(-10*time.Minute))
	seedFailedRun(ctx, log, "demo-failed", now.Add(-7*time.Minute))
	seedCancelledRun(ctx, log, "demo-cancelled", now.Add(-4*time.Minute))
	seedInProgressRun(ctx, log, "demo-in-progress", now.Add(-1*time.Minute))

	fmt.Printf("seeded 4 runs into %s\n", path)
	fmt.Printf("now run:  go run ./cmd/starling-inspect %s\n", path)
}

// seedHappyRun produces a fully-valid run: real PrevHash chain, real
// Merkle root in the terminal payload. Validate() returns nil and the
// inspector shows a green "✓ chain valid" badge.
func seedHappyRun(ctx context.Context, log eventlog.EventLog, runID string, base time.Time) {
	w := newWriter(log, runID, base)
	w.append(event.KindRunStarted, event.RunStarted{
		ModelID: "gpt-4o-mini", ProviderID: "openai", Goal: "say hi politely",
	})
	w.append(event.KindUserMessageAppended, event.UserMessageAppended{
		Content: "hello, what time is it?",
	})
	w.append(event.KindTurnStarted, event.TurnStarted{
		TurnID: "t1", InputTokens: 42,
	})
	w.append(event.KindToolCallScheduled, event.ToolCallScheduled{
		CallID: "c1", TurnID: "t1", ToolName: "current_time", Attempt: 1,
	})
	w.append(event.KindToolCallCompleted, event.ToolCallCompleted{
		CallID: "c1", DurationMs: 18, Attempt: 1,
	})
	w.append(event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{
		TurnID: "t1", Text: "It's 3:14pm.", StopReason: "end_turn",
		InputTokens: 50, OutputTokens: 8, CostUSD: 0.0001,
	})
	w.finishCompleted(event.RunCompleted{
		FinalText: "It's 3:14pm.", TurnCount: 1, ToolCallCount: 1, DurationMs: 750,
	})
}

// seedFailedRun terminates with KindRunFailed and a real Merkle root.
func seedFailedRun(ctx context.Context, log eventlog.EventLog, runID string, base time.Time) {
	w := newWriter(log, runID, base)
	w.append(event.KindRunStarted, event.RunStarted{
		ModelID: "gpt-4o-mini", ProviderID: "openai", Goal: "fetch a flaky URL",
	})
	w.append(event.KindUserMessageAppended, event.UserMessageAppended{
		Content: "fetch https://example.invalid",
	})
	w.append(event.KindTurnStarted, event.TurnStarted{TurnID: "t1", InputTokens: 38})
	w.append(event.KindToolCallScheduled, event.ToolCallScheduled{
		CallID: "c1", TurnID: "t1", ToolName: "fetch", Attempt: 1,
	})
	w.append(event.KindToolCallFailed, event.ToolCallFailed{
		CallID: "c1", Error: "no such host", ErrorType: "network", DurationMs: 234, Attempt: 1,
	})
	w.finishFailed(event.RunFailed{
		Error: "tool fetch failed after 1 attempt", ErrorType: "tool_failure", DurationMs: 600,
	})
}

// seedCancelledRun terminates with KindRunCancelled.
func seedCancelledRun(ctx context.Context, log eventlog.EventLog, runID string, base time.Time) {
	w := newWriter(log, runID, base)
	w.append(event.KindRunStarted, event.RunStarted{
		ModelID: "gpt-4o-mini", ProviderID: "openai", Goal: "long task",
	})
	w.append(event.KindTurnStarted, event.TurnStarted{TurnID: "t1", InputTokens: 100})
	w.finishCancelled(event.RunCancelled{Reason: "user_interrupt", DurationMs: 1200})
}

// seedInProgressRun never appends a terminal event. The inspector lists
// it under the "in progress" status filter; the validation badge will
// show "chain invalid: last event kind ... is not terminal" — the
// expected reading on a half-finished run.
func seedInProgressRun(ctx context.Context, log eventlog.EventLog, runID string, base time.Time) {
	w := newWriter(log, runID, base)
	w.append(event.KindRunStarted, event.RunStarted{
		ModelID: "gpt-4o-mini", ProviderID: "openai", Goal: "still going",
	})
	w.append(event.KindUserMessageAppended, event.UserMessageAppended{
		Content: "do the thing",
	})
	w.append(event.KindTurnStarted, event.TurnStarted{TurnID: "t1", InputTokens: 12})
	w.append(event.KindToolCallScheduled, event.ToolCallScheduled{
		CallID: "c1", TurnID: "t1", ToolName: "fetch", Attempt: 1,
	})
	// Deliberately no completion / no terminal — the run is "stuck".
}

// runWriter accumulates per-run state (Seq, PrevHash, list of marshaled
// envelopes) so the terminal Merkle root can be computed inline.
type runWriter struct {
	ctx     context.Context
	log     eventlog.EventLog
	runID   string
	base    time.Time
	seq     uint64
	prev    []byte
	written []event.Event
}

func newWriter(log eventlog.EventLog, runID string, base time.Time) *runWriter {
	return &runWriter{ctx: context.Background(), log: log, runID: runID, base: base}
}

func (w *runWriter) append(kind event.Kind, payload any) {
	w.seq++
	b, err := event.EncodePayload(payload)
	if err != nil {
		die("EncodePayload(%v): %v", kind, err)
	}
	ev := event.Event{
		RunID:     w.runID,
		Seq:       w.seq,
		PrevHash:  w.prev,
		Timestamp: w.base.Add(time.Duration(w.seq) * 100 * time.Millisecond).UnixNano(),
		Kind:      kind,
		Payload:   b,
	}
	enc, err := event.Marshal(ev)
	if err != nil {
		die("Marshal: %v", err)
	}
	if err := w.log.Append(w.ctx, w.runID, ev); err != nil {
		die("Append: %v", err)
	}
	w.prev = event.Hash(enc)
	w.written = append(w.written, ev)
}

// merkleRoot returns the Merkle root over every event written so far.
// Used by finish* to populate the terminal payload's MerkleRoot field.
func (w *runWriter) merkleRoot() []byte {
	leaves, err := merkle.EventHashes(w.written)
	if err != nil {
		die("EventHashes: %v", err)
	}
	return merkle.Root(leaves)
}

func (w *runWriter) finishCompleted(p event.RunCompleted) {
	p.MerkleRoot = w.merkleRoot()
	w.append(event.KindRunCompleted, p)
}

func (w *runWriter) finishFailed(p event.RunFailed) {
	p.MerkleRoot = w.merkleRoot()
	w.append(event.KindRunFailed, p)
}

func (w *runWriter) finishCancelled(p event.RunCancelled) {
	p.MerkleRoot = w.merkleRoot()
	w.append(event.KindRunCancelled, p)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "m4_inspector_demo: "+format+"\n", args...)
	os.Exit(1)
}
