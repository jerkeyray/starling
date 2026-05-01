// Package cborenc is Starling's canonical CBOR codec (RFC 8949 §4.2).
// All event-log encoding goes through here; bypassing it via
// fxamacker/cbor directly breaks hash-chain stability.
package cborenc

import "github.com/fxamacker/cbor/v2"

// RawMessage is a CBOR value held as verbatim bytes. Re-exported so
// payloads can round-trip through the event envelope without re-encoding.
type RawMessage = cbor.RawMessage

var (
	enc cbor.EncMode
	dec cbor.DecMode
)

func init() {
	e, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic("cborenc: canonical enc mode: " + err.Error())
	}
	enc = e

	// Strict decoder: duplicate map keys and indefinite-length items
	// are never produced by our encoder, and accepting them would let
	// a corrupted or malicious log decode to something that no longer
	// hashes to the committed PrevHash.
	d, err := cbor.DecOptions{
		DupMapKey:   cbor.DupMapKeyEnforcedAPF,
		IndefLength: cbor.IndefLengthForbidden,
	}.DecMode()
	if err != nil {
		panic("cborenc: strict dec mode: " + err.Error())
	}
	dec = d
}

// Marshal encodes v as canonical CBOR (RFC 8949 §4.2). Semantically
// equal inputs produce byte-identical output.
func Marshal(v any) ([]byte, error) { return enc.Marshal(v) }

// Unmarshal decodes canonical CBOR into v. Rejects duplicate map keys
// and indefinite-length items.
func Unmarshal(data []byte, v any) error { return dec.Unmarshal(data, v) }
