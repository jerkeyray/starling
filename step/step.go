// Package step is the determinism boundary. Non-deterministic ops
// (clock, randomness, LLM, tools) route through this package so the
// runtime can record and replay them.
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

// Name values used in SideEffectRecorded for built-in helpers.
// User keys must avoid "now"/"rand".
const (
	ndetNameNow  = "now"
	ndetNameRand = "rand"
)

// Now returns the current wall-clock time. Live mode reads
// Context.clockFn and records the value as nanoseconds; replay mode
// returns the recorded value. Panics if ctx has no step.Context or
// (in replay) if the next side effect isn't "now".
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
		// The event log rejected a SideEffectRecorded write. The run's
		// hash chain is now incomplete, so any further work would
		// produce a non-replayable trace — fail loudly so operators
		// see the underlying log-backend fault instead of silently
		// continuing into corruption.
		panic(fmt.Sprintf("step.Now: event log rejected SideEffectRecorded write (run is now non-replayable): %v", err))
	}
	return t
}

// Random returns a cryptographically random uint64. Live mode draws
// from crypto/rand and records as SideEffectRecorded{name:"rand"};
// replay returns the recorded value. Panics on missing ctx,
// CSPRNG failure, or replay mismatch.
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
		panic(fmt.Sprintf("step.Random: event log rejected SideEffectRecorded write (run is now non-replayable): %v", err))
	}
	return v
}

// SideEffect records arbitrary non-determinism: HTTP, filesystem,
// anything beyond clock/RNG. Live mode runs fn and records its
// result under name; replay decodes the recorded value without
// invoking fn. fn errors are propagated unrecorded (replay re-runs
// fn). T must be CBOR-serialisable. Panics on missing ctx or replay
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

// replayInt64 pops the next SideEffectRecorded, asserts the name
// matches, and decodes as int64 (falling back to uint64 for Random).
// Panics on mismatch; the replay verifier recovers.
func replayInt64(c *Context, want string) int64 {
	raw := replayRaw(c, want)
	var out int64
	if err := cborenc.Unmarshal(raw, &out); err != nil {
		// uint64 fallback for Random.
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
