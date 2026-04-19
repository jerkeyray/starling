// Package merkle provides the binary BLAKE3 Merkle-tree helpers used to
// commit to a run's event log. The root is embedded in every terminal
// event so any tampering with a prior event breaks the commitment.
//
// The implementation is deliberately simple: pairwise-hash up the tree
// with Bitcoin-style odd-duplication at each level. Empty input yields
// a 32-byte zero digest so terminal events always carry a fixed-width
// field.
package merkle

import (
	"github.com/jerkeyray/starling/event"
	"github.com/zeebo/blake3"
)

// Root computes a binary Merkle root over the given leaf hashes. Each
// leaf is expected to be a BLAKE3-32 digest of one canonical-CBOR event
// envelope (i.e. event.Hash(event.Marshal(ev))).
//
// Empty input returns a 32-byte zero digest. Odd-length levels
// duplicate the last node.
func Root(leaves [][]byte) []byte {
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
			next = append(next, h.Sum(nil))
		}
		level = next
	}
	return level[0]
}

// EventHashes collects per-event leaf hashes in the order they appear
// in evs. Used to produce Merkle input from a stored event slice.
func EventHashes(evs []event.Event) ([][]byte, error) {
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
