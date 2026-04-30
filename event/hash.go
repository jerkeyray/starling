package event

import "github.com/zeebo/blake3"

// HashSize is the length in bytes of a Hash output.
const HashSize = 32

// Hash returns the 32-byte BLAKE3 digest of canonical CBOR bytes. The chain
// invariant is event[i+1].PrevHash = Hash(Marshal(event[i])), with
// event[0].PrevHash == nil. Pass non-canonical bytes and the digest will not
// be reproducible from the logical value.
func Hash(cborBytes []byte) []byte {
	sum := blake3.Sum256(cborBytes)
	return sum[:]
}
