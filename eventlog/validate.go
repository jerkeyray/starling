package eventlog

import (
	"bytes"
	"fmt"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/internal/merkle"
)

// Validate verifies the integrity of a full run's event slice. The
// events must be in the order they were appended. Validate checks:
//
//  1. The slice is non-empty.
//  2. events[0].Seq == 1 and every subsequent Seq increments by one.
//  3. RunID is identical and non-empty across all events.
//  4. events[0].PrevHash is empty/nil; each later PrevHash equals
//     event.Hash(event.Marshal(prev)).
//  5. Exactly one terminal event (RunCompleted/RunFailed/RunCancelled)
//     appears, and only as the last event.
//  6. The terminal payload's MerkleRoot equals the Merkle root over
//     every pre-terminal event's canonical-CBOR hash.
//
// On success returns nil. On failure returns an error wrapping
// ErrLogCorrupt with a concise diagnostic.
func Validate(events []event.Event) error {
	if len(events) == 0 {
		return fmt.Errorf("%w: no events", ErrLogCorrupt)
	}
	if err := validateEnvelopes(events); err != nil {
		return err
	}
	if err := validateTerminal(events); err != nil {
		return err
	}
	return validateMerkleRoot(events)
}

// validateEnvelopes walks the events once, checking seq monotonicity,
// RunID consistency, and the hash-chain linkage.
func validateEnvelopes(events []event.Event) error {
	runID := events[0].RunID
	if runID == "" {
		return fmt.Errorf("%w: event[0] has empty RunID", ErrLogCorrupt)
	}
	if events[0].Seq != 1 {
		return fmt.Errorf("%w: event[0].Seq = %d, want 1", ErrLogCorrupt, events[0].Seq)
	}
	if len(events[0].PrevHash) != 0 {
		return fmt.Errorf("%w: event[0].PrevHash is non-empty", ErrLogCorrupt)
	}

	for i := 1; i < len(events); i++ {
		prev := events[i-1]
		cur := events[i]
		if cur.RunID != runID {
			return fmt.Errorf("%w: RunID mismatch at index %d: %q vs %q", ErrLogCorrupt, i, cur.RunID, runID)
		}
		if cur.Seq != prev.Seq+1 {
			return fmt.Errorf("%w: seq gap at index %d: want %d got %d", ErrLogCorrupt, i, prev.Seq+1, cur.Seq)
		}
		prevBytes, err := event.Marshal(prev)
		if err != nil {
			return fmt.Errorf("%w: marshal event[%d]: %v", ErrLogCorrupt, i-1, err)
		}
		want := event.Hash(prevBytes)
		if !bytes.Equal(cur.PrevHash, want) {
			return fmt.Errorf("%w: prev-hash mismatch at index %d", ErrLogCorrupt, i)
		}
	}
	return nil
}

// validateTerminal verifies that exactly one terminal event is present
// and that it occupies the final slot.
func validateTerminal(events []event.Event) error {
	last := events[len(events)-1]
	if !last.Kind.IsTerminal() {
		return fmt.Errorf("%w: last event kind %s is not terminal", ErrLogCorrupt, last.Kind)
	}
	for i := 0; i < len(events)-1; i++ {
		if events[i].Kind.IsTerminal() {
			return fmt.Errorf("%w: terminal event at index %d before end", ErrLogCorrupt, i)
		}
	}
	return nil
}

// validateMerkleRoot computes the Merkle root over every pre-terminal
// event and compares it to the root stored in the terminal payload.
func validateMerkleRoot(events []event.Event) error {
	last := events[len(events)-1]
	stored, err := terminalMerkleRoot(last)
	if err != nil {
		return fmt.Errorf("%w: decode terminal payload: %v", ErrLogCorrupt, err)
	}

	leaves, err := merkle.EventHashes(events[:len(events)-1])
	if err != nil {
		return fmt.Errorf("%w: hash pre-terminal events: %v", ErrLogCorrupt, err)
	}
	want := merkle.Root(leaves)
	if !bytes.Equal(stored, want) {
		return fmt.Errorf("%w: merkle root mismatch", ErrLogCorrupt)
	}
	return nil
}

// terminalMerkleRoot pulls MerkleRoot out of the terminal event's
// payload regardless of which of the three terminal kinds it is.
func terminalMerkleRoot(ev event.Event) ([]byte, error) {
	switch ev.Kind {
	case event.KindRunCompleted:
		rc, err := ev.AsRunCompleted()
		if err != nil {
			return nil, err
		}
		return rc.MerkleRoot, nil
	case event.KindRunFailed:
		rf, err := ev.AsRunFailed()
		if err != nil {
			return nil, err
		}
		return rf.MerkleRoot, nil
	case event.KindRunCancelled:
		rc, err := ev.AsRunCancelled()
		if err != nil {
			return nil, err
		}
		return rc.MerkleRoot, nil
	}
	return nil, fmt.Errorf("kind %s is not terminal", ev.Kind)
}
