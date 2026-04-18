package event_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/internal/cborenc"
)

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func TestHash_Length(t *testing.T) {
	h := event.Hash([]byte("anything"))
	if len(h) != event.HashSize {
		t.Fatalf("expected %d bytes, got %d", event.HashSize, len(h))
	}
}

func TestHash_Deterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		in := randBytes(t, 64)
		a := event.Hash(in)
		b := event.Hash(in)
		if !bytes.Equal(a, b) {
			t.Fatalf("iter %d: hash non-deterministic\na=% x\nb=% x", i, a, b)
		}
	}
}

func TestHash_Distinguishes(t *testing.T) {
	for i := 0; i < 100; i++ {
		x := randBytes(t, 64)
		y := randBytes(t, 64)
		if bytes.Equal(x, y) {
			continue // vanishingly unlikely, but skip to keep the assertion meaningful
		}
		if bytes.Equal(event.Hash(x), event.Hash(y)) {
			t.Fatalf("iter %d: distinct inputs produced identical hash", i)
		}
	}
}

// stubEvent stands in for the real Event envelope, which lands in T3. It has
// enough shape to exercise the chain: a sequence number, a PrevHash slot
// filled by the caller, and some body bytes that change per event.
type stubEvent struct {
	Seq      uint64 `cbor:"seq"`
	PrevHash []byte `cbor:"prev_hash"`
	Body     []byte `cbor:"body"`
}

func buildChain(t *testing.T, n int) []stubEvent {
	t.Helper()
	chain := make([]stubEvent, n)
	var prev []byte
	for i := 0; i < n; i++ {
		chain[i] = stubEvent{
			Seq:      uint64(i),
			PrevHash: prev,
			Body:     randBytes(t, 32),
		}
		enc, err := cborenc.Marshal(chain[i])
		if err != nil {
			t.Fatalf("marshal event %d: %v", i, err)
		}
		prev = event.Hash(enc)
	}
	return chain
}

func recomputeChain(t *testing.T, chain []stubEvent) [][]byte {
	t.Helper()
	hashes := make([][]byte, len(chain))
	var prev []byte
	for i, ev := range chain {
		if !bytes.Equal(ev.PrevHash, prev) {
			t.Fatalf("chain break at index %d:\nstored PrevHash=% x\nexpected      =% x", i, ev.PrevHash, prev)
		}
		enc, err := cborenc.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event %d: %v", i, err)
		}
		prev = event.Hash(enc)
		hashes[i] = prev
	}
	return hashes
}

func TestHash_Chain(t *testing.T) {
	chain := buildChain(t, 100)

	// Fresh recomputation should succeed and produce the same hash trail.
	original := recomputeChain(t, chain)
	again := recomputeChain(t, chain)
	for i := range original {
		if !bytes.Equal(original[i], again[i]) {
			t.Fatalf("chain recomputation non-deterministic at index %d", i)
		}
	}
}

// walkChain returns the first index where chain[i].PrevHash disagrees with
// the recomputed hash of chain[i-1] (or -1 if the whole chain is consistent).
func walkChain(t *testing.T, chain []stubEvent) int {
	t.Helper()
	var prev []byte
	for i, ev := range chain {
		if !bytes.Equal(ev.PrevHash, prev) {
			return i
		}
		enc, err := cborenc.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event %d: %v", i, err)
		}
		prev = event.Hash(enc)
	}
	return -1
}

func TestHash_Chain_TamperDetected(t *testing.T) {
	chain := buildChain(t, 100)

	// Fresh, untampered chain must walk cleanly end-to-end.
	if idx := walkChain(t, chain); idx != -1 {
		t.Fatalf("untampered chain broke at index %d", idx)
	}

	// Flip one byte in the body of a middle event. Its own hash now differs
	// from what event 43's stored PrevHash was computed against.
	chain[42].Body[0] ^= 0xff

	// The walker should detect the break at index 43 — the first event whose
	// stored PrevHash no longer matches the recomputed hash of its predecessor.
	// Link *into* 42 is still fine (chain[42].PrevHash wasn't touched), so the
	// break surfaces one step later.
	if idx := walkChain(t, chain); idx != 43 {
		t.Fatalf("expected tamper detected at index 43, got %d", idx)
	}
}
