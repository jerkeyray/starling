// Package cborenc provides canonical CBOR encoding (RFC 8949 §4.2) used by
// the event log for hash-chain stability. Internal: import paths are not
// part of the public API.
//
// Every CBOR call in Starling goes through this package. Reaching for
// fxamacker/cbor directly would bypass deterministic-encoding mode and break
// the hash chain — so we funnel everything through Marshal/Unmarshal here.
package cborenc

import "github.com/fxamacker/cbor/v2"

// RawMessage is a CBOR value held as verbatim bytes. Re-exported from
// fxamacker/cbor/v2 so callers never import that package directly — it is
// the one type (not a function) that must cross the cborenc boundary in
// order for event payloads to round-trip without re-encoding.
type RawMessage = cbor.RawMessage

var (
	enc cbor.EncMode
	dec cbor.DecMode
)

func init() {
	// RFC 8949 §4.2 Core Deterministic Encoding Requirements:
	//   - shortest-form integer encoding
	//   - shortest-form floating-point encoding, NaN normalized to 0x7e00
	//   - map keys sorted bytewise lexical on their CBOR-encoded form
	//   - definite-length encoding only (no indefinite-length)
	e, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic("cborenc: canonical enc mode: " + err.Error())
	}
	enc = e

	// Decoder: be strict — duplicate map keys and indefinite-length items are
	// never produced by our encoder, and accepting them would let a corrupted
	// or malicious log decode to something that no longer hashes to the
	// committed PrevHash.
	d, err := cbor.DecOptions{
		DupMapKey:   cbor.DupMapKeyEnforcedAPF,
		IndefLength: cbor.IndefLengthForbidden,
	}.DecMode()
	if err != nil {
		panic("cborenc: strict dec mode: " + err.Error())
	}
	dec = d
}

// Marshal encodes v using RFC 8949 §4.2 Core Deterministic CBOR.
// Calling Marshal on semantically equal inputs produces byte-identical output.
func Marshal(v any) ([]byte, error) { return enc.Marshal(v) }

// Unmarshal decodes canonical CBOR bytes into v. Rejects duplicate map keys
// and indefinite-length items.
func Unmarshal(data []byte, v any) error { return dec.Unmarshal(data, v) }
