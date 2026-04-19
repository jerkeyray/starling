package starling

import (
	"github.com/jerkeyray/starling/event"
	"github.com/zeebo/blake3"
)

// merkleRoot computes a binary Merkle root over the given leaf hashes.
// Each leaf is expected to be a BLAKE3-32 digest of one canonical CBOR
// event envelope (i.e. event.Hash(event.Marshal(ev))).
//
// Empty input returns a 32-byte zero digest so terminal events always
// have a fixed-width field. Odd-length levels duplicate the last node
// (standard Bitcoin-style Merkle); this keeps the implementation
// trivial and deterministic while remaining straightforward to verify.
//
// The root lives in the terminal event's MerkleRoot field and commits
// to every prior event in the run — tampering with any predecessor
// changes the root.
func merkleRoot(leaves [][]byte) []byte {
	if len(leaves) == 0 {
		return make([]byte, 32)
	}
	level := make([][]byte, len(leaves))
	copy(level, leaves)

	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, level[len(level)-1])
		}
		next := make([][]byte, 0, len(level)/2)
		for i := 0; i < len(level); i += 2 {
			h := blake3.New()
			_, _ = h.Write(level[i])
			_, _ = h.Write(level[i+1])
			sum := h.Sum(nil)
			next = append(next, sum)
		}
		level = next
	}
	return level[0]
}

// eventHashes collects the per-event leaf hashes in the order they
// appear in evs. Used by the agent loop to produce the Merkle root
// over the events emitted so far when it's time to write a terminal.
func eventHashes(evs []event.Event) ([][]byte, error) {
	hashes := make([][]byte, 0, len(evs))
	for i := range evs {
		b, err := event.Marshal(evs[i])
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, event.Hash(b))
	}
	return hashes, nil
}
