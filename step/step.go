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
	"github.com/jerkeyray/starling/internal/cborenc"
)

// Name values used in SideEffectRecorded for the built-in helpers.
// User-supplied keys flow through SideEffect with no prefix — callers
// are responsible for avoiding "now"/"rand".
const (
	ndetNameNow  = "now"
	ndetNameRand = "rand"
)

// Now returns the current wall-clock time. In live mode it reads the
// Context's clock (defaults to time.Now) and records the value as
// nanoseconds since the Unix epoch. In replay mode it returns the
// recorded value without consulting the clock.
//
// Panics if ctx has no step.Context attached (a non-deterministic
// value produced without an emitted event makes the run non-replayable,
// which is a programmer bug). Also panics in replay mode if the next
// recorded side effect isn't a "now" — the replay verifier recovers
// these panics and surfaces starling.ErrNonDeterminism.
func Now(ctx context.Context) time.Time {
	c := mustFrom(ctx, "Now")
	if c.mode == ModeReplay {
		nanos := replayInt64(c, ndetNameNow)
		return time.Unix(0, nanos)
	}
	t := c.clockFn()
	if err := emit(ctx, c, event.KindSideEffectRecorded, event.SideEffectRecorded{
		Name:  ndetNameNow,
		Value: mustEncode(t.UnixNano()),
	}); err != nil {
		panic(fmt.Sprintf("step.Now: emit: %v", err))
	}
	return t
}

// Random returns a cryptographically random uint64. In live mode the
// value is drawn from crypto/rand and recorded as a SideEffectRecorded
// event with name="rand". In replay mode the recorded value is
// returned without touching the CSPRNG.
//
// Panics if ctx has no step.Context attached (see Now for rationale),
// if the system's CSPRNG fails, or on a replay-stream mismatch.
func Random(ctx context.Context) uint64 {
	c := mustFrom(ctx, "Random")
	if c.mode == ModeReplay {
		return uint64(replayInt64(c, ndetNameRand))
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("step.Random: crypto/rand: %v", err))
	}
	v := binary.BigEndian.Uint64(buf[:])
	if err := emit(ctx, c, event.KindSideEffectRecorded, event.SideEffectRecorded{
		Name:  ndetNameRand,
		Value: mustEncode(v),
	}); err != nil {
		panic(fmt.Sprintf("step.Random: emit: %v", err))
	}
	return v
}

// SideEffect is the escape hatch for arbitrary non-determinism: HTTP
// calls, filesystem reads, anything a tool might do that's not a clock
// or an RNG. In live mode it runs fn and records the result as a
// SideEffectRecorded event under name. In replay mode it decodes the
// recorded value and returns it without invoking fn.
//
// On fn error in live mode the error is propagated and no event is
// emitted — replay can re-run fn deterministically. T must be
// CBOR-serialisable.
//
// Panics if ctx has no step.Context attached, or on a replay-stream
// mismatch.
func SideEffect[T any](ctx context.Context, name string, fn func() (T, error)) (T, error) {
	c := mustFrom(ctx, "SideEffect")
	if c.mode == ModeReplay {
		raw := replayRaw(c, name)
		var out T
		if err := cborenc.Unmarshal(raw, &out); err != nil {
			panic(fmt.Sprintf("step.SideEffect(%q): replay decode: %v", name, err))
		}
		return out, nil
	}
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

// replayInt64 pops the next SideEffectRecorded from c.recorded, asserts
// the name matches want, and decodes its value as int64 (or uint64
// via caller conversion). Panics with an ErrReplayMismatch-wrapped
// message on any discrepancy — the replay verifier (T17) recovers and
// converts to starling.ErrNonDeterminism.
func replayInt64(c *Context, want string) int64 {
	raw := replayRaw(c, want)
	var out int64
	if err := cborenc.Unmarshal(raw, &out); err != nil {
		// uint64 is emitted by Random; decode into that as a fallback
		// so both Now (int64 nanos) and Random (uint64) can share the
		// code path without a type tag.
		var u uint64
		if uErr := cborenc.Unmarshal(raw, &u); uErr == nil {
			return int64(u)
		}
		panic(fmt.Sprintf("%v: %s decode: %v", ErrReplayMismatch, want, err))
	}
	return out
}

// replayRaw advances c.replayIdx to the next SideEffectRecorded event,
// asserts its Name matches want, and returns the raw CBOR value bytes.
// Called from helpers that run in replay mode; panics on mismatch.
func replayRaw(c *Context, want string) cborenc.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.replayIdx < len(c.recorded) {
		ev := c.recorded[c.replayIdx]
		c.replayIdx++
		if ev.Kind != event.KindSideEffectRecorded {
			continue
		}
		payload, err := ev.AsSideEffectRecorded()
		if err != nil {
			panic(fmt.Sprintf("%v: decode SideEffectRecorded at seq=%d: %v", ErrReplayMismatch, ev.Seq, err))
		}
		if payload.Name != want {
			panic(fmt.Sprintf("%v: expected name %q, got %q at seq=%d", ErrReplayMismatch, want, payload.Name, ev.Seq))
		}
		return payload.Value
	}
	panic(fmt.Sprintf("%v: no SideEffectRecorded remaining (wanted %q)", ErrReplayMismatch, want))
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
