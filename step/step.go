// Package step is the determinism boundary. Non-deterministic operations
// the agent loop would otherwise perform directly — reading the clock,
// generating randomness, calling the LLM, invoking a tool — go through
// functions in this package so the runtime can record their results in
// the event log and replay them later.
//
// Named "step" (not "runtime") to avoid collision with the stdlib runtime
// package.
package step

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jerkeyray/starling/event"
)

// Now returns the current wall-clock time and records it as a
// SideEffectRecorded event with name="now" and value=UnixNano.
//
// Panics if ctx has no step.Context attached: a non-deterministic value
// produced without an emitted event makes the run non-replayable, and
// that is always a programmer bug.
func Now(ctx context.Context) time.Time {
	c := mustFrom(ctx, "Now")
	t := time.Now()
	if err := emit(ctx, c, event.KindSideEffectRecorded, event.SideEffectRecorded{
		Name:  "now",
		Value: mustEncode(t.UnixNano()),
	}); err != nil {
		panic(fmt.Sprintf("step.Now: emit: %v", err))
	}
	return t
}

// Random returns a cryptographically random uint64 and records it as a
// SideEffectRecorded event with name="rand".
//
// Panics if ctx has no step.Context attached (see Now for rationale) or
// if the system's CSPRNG fails — the latter is a platform-level failure
// not recoverable at this layer.
func Random(ctx context.Context) uint64 {
	c := mustFrom(ctx, "Random")
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("step.Random: crypto/rand: %v", err))
	}
	v := binary.BigEndian.Uint64(buf[:])
	if err := emit(ctx, c, event.KindSideEffectRecorded, event.SideEffectRecorded{
		Name:  "rand",
		Value: mustEncode(v),
	}); err != nil {
		panic(fmt.Sprintf("step.Random: emit: %v", err))
	}
	return v
}

// SideEffect runs fn and, on success, records its result as a
// SideEffectRecorded event with the given name and the CBOR-encoded
// return value. On fn error, the error is propagated unchanged and no
// event is emitted — replay can re-run fn deterministically.
//
// Panics if ctx has no step.Context attached.
func SideEffect[T any](ctx context.Context, name string, fn func() (T, error)) (T, error) {
	c := mustFrom(ctx, "SideEffect")
	out, err := fn()
	if err != nil {
		var zero T
		return zero, err
	}
	encoded, encErr := event.EncodePayload(out)
	if encErr != nil {
		return out, fmt.Errorf("step.SideEffect(%q): encode result: %w", name, encErr)
	}
	if emitErr := emit(ctx, c, event.KindSideEffectRecorded, event.SideEffectRecorded{
		Name:  name,
		Value: encoded,
	}); emitErr != nil {
		return out, fmt.Errorf("step.SideEffect(%q): emit: %w", name, emitErr)
	}
	return out, nil
}

// mustEncode CBOR-encodes v and panics on failure. Used by Now/Random
// where the value types are known-simple (int64, uint64) and cannot
// realistically fail to marshal.
func mustEncode(v any) []byte {
	b, err := event.EncodePayload(v)
	if err != nil {
		panic(fmt.Sprintf("step: encode %T: %v", v, err))
	}
	return b
}
