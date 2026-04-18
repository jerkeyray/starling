package event

import "github.com/zeebo/blake3"

// HashSize is the length in bytes of a Hash output.
const HashSize = 32

// Hash returns the 32-byte BLAKE3 digest of the given canonical CBOR bytes.
//
// The Starling event-log chain is defined as:
//
//	event[i+1].PrevHash = Hash(cborenc.Marshal(event[i]))
//
// with event[0].PrevHash == nil. The chain link is carried by each event's
// own PrevHash field; this function is just the cryptographic primitive.
//
// Callers are expected to pass canonical CBOR bytes (from
// internal/cborenc.Marshal). Passing non-canonical bytes will still produce
// a hash, but it will not be reproducible from the logical value.
func Hash(cborBytes []byte) []byte {
	sum := blake3.Sum256(cborBytes)
	return sum[:]
}
