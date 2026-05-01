// Command manual_writes shows how to write events into a Starling
// event log without using Agent.Run — useful when integrating
// non-LLM workflows that nonetheless want the same audit log,
// inspector, and validation surface.
//
// Pattern: build event.Event values yourself, compute the BLAKE3
// chain hash via event.Hash(event.Marshal(prev)), and use the
// public merkle package for the terminal Merkle root. The runtime
// validators, the inspector, and the replay package treat your log
// identically to one produced by Agent.Run as long as you preserve
// the chain invariants.
//
// Run:
//
//	go run ./examples/manual_writes
//
// Inspect with:
//
//	starling-inspect manual.db
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/merkle"
)

func main() {
	const path = "manual.db"
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")

	store, err := eventlog.NewSQLite(path)
	if err != nil {
		log.Fatalf("open log: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	const runID = "manual-run-001"

	// 1. RunStarted: seq=1, no PrevHash. SchemaVersion is required so
	//    Resume and Replay agree on the on-disk format.
	rs := event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          "ETL: import 3 source rows into the audit log",
		ProviderID:    "manual-writer",
		APIVersion:    "v0",
		ModelID:       "n/a",
	}
	prev := mustAppend(ctx, store, runID, 1, nil, event.KindRunStarted, rs)

	// 2. SideEffectRecorded: anything you want to commit to the chain.
	//    Value must be valid canonical CBOR — encode your payload
	//    through cbor.Marshal so the chain hash is reproducible.
	for i, val := range []string{"row-A", "row-B", "row-C"} {
		raw, err := cbor.Marshal(map[string]string{"row": val})
		if err != nil {
			log.Fatalf("encode value: %v", err)
		}
		se := event.SideEffectRecorded{Name: "imported", Value: raw}
		prev = mustAppend(ctx, store, runID, uint64(i+2),
			event.Hash(mustMarshal(prev)),
			event.KindSideEffectRecorded, se)
	}

	// 3. Terminal: RunCompleted with a Merkle root over every prior
	//    event's hash. The merkle package is the same one the runtime
	//    uses, so consumers can recompute the root and verify it.
	hashes := chainHashes(ctx, store, runID)
	root := merkle.Root(hashes)

	rc := event.RunCompleted{
		FinalText:         "imported 3 rows",
		TurnCount:         0,
		ToolCallCount:     0,
		TotalCostUSD:      0,
		TotalInputTokens:  0,
		TotalOutputTokens: 0,
		DurationMs:        0,
		MerkleRoot:        root,
	}
	mustAppend(ctx, store, runID, prev.Seq+1,
		event.Hash(mustMarshal(prev)),
		event.KindRunCompleted, rc)

	// 4. Verify: eventlog.Validate walks the chain and reports the
	//    first break. A clean log returns nil.
	evs, err := store.Read(ctx, runID)
	if err != nil {
		log.Fatalf("Read: %v", err)
	}
	if err := eventlog.Validate(evs); err != nil {
		log.Fatalf("Validate: %v", err)
	}
	fmt.Printf("wrote %d events into %s, chain valid, merkle=%x\n", len(evs), path, root[:8])
}

// mustAppend builds an event.Event, appends it, and returns it for
// the next iteration's PrevHash computation. Fatal-exits on any error
// so the example stays linear; production code should propagate.
func mustAppend(ctx context.Context, log eventlog.EventLog, runID string, seq uint64, prevHash []byte, kind event.Kind, payload any) event.Event {
	raw, err := encodePayload(kind, payload)
	if err != nil {
		fatal("encode %s: %v", kind, err)
	}
	ev := event.Event{
		RunID:     runID,
		Seq:       seq,
		PrevHash:  prevHash,
		Timestamp: time.Now().UnixNano(),
		Kind:      kind,
		Payload:   raw,
	}
	if err := log.Append(ctx, runID, ev); err != nil {
		fatal("Append seq=%d: %v", seq, err)
	}
	return ev
}

// chainHashes returns event.Hash(event.Marshal(ev)) for every event
// already in the log under runID — i.e. the leaves the terminal
// MerkleRoot commits to.
func chainHashes(ctx context.Context, log eventlog.EventLog, runID string) [][]byte {
	evs, err := log.Read(ctx, runID)
	if err != nil {
		fatal("chainHashes Read: %v", err)
	}
	out := make([][]byte, len(evs))
	for i, ev := range evs {
		out[i] = event.Hash(mustMarshal(ev))
	}
	return out
}

func encodePayload(kind event.Kind, payload any) ([]byte, error) {
	switch p := payload.(type) {
	case event.RunStarted:
		return event.EncodePayload(p)
	case event.SideEffectRecorded:
		return event.EncodePayload(p)
	case event.RunCompleted:
		return event.EncodePayload(p)
	default:
		return nil, fmt.Errorf("manual_writes: unsupported kind %s", kind)
	}
}

func mustMarshal(ev event.Event) []byte {
	b, err := event.Marshal(ev)
	if err != nil {
		fatal("Marshal: %v", err)
	}
	return b
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
