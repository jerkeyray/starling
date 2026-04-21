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
// returns the recorded value. In both modes a SideEffectRecorded event
// is emitted so replay re-appends an identical chain. Panics if ctx
// has no step.Context, the event log rejects the write, or (in replay)
// the next recorded event doesn't match.
func Now(ctx context.Context) time.Time {
	c := mustFrom(ctx, "Now")
	var nanos int64
	if c.mode == ModeReplay {
		raw, err := peekReplaySideEffect(c, ndetNameNow)
		if err != nil {
			panic(err.Error())
		}
		if err := cborenc.Unmarshal(raw, &nanos); err != nil {
			panic(fmt.Sprintf("%v: decode now: %v", ErrReplayMismatch, err))
		}
		if err := emit(ctx, c, event.KindSideEffectRecorded, event.SideEffectRecorded{
			Name:  ndetNameNow,
			Value: raw,
		}); err != nil {
			panic(fmt.Sprintf("step.Now: replay emit: %v", err))
		}
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
// replay returns the recorded value. A SideEffectRecorded event is
// emitted in both modes. Panics on missing ctx, CSPRNG failure, or
// replay mismatch.
func Random(ctx context.Context) uint64 {
	c := mustFrom(ctx, "Random")
	if c.mode == ModeReplay {
		raw, err := peekReplaySideEffect(c, ndetNameRand)
		if err != nil {
			panic(err.Error())
		}
		var v uint64
		if err := cborenc.Unmarshal(raw, &v); err != nil {
			panic(fmt.Sprintf("%v: decode rand: %v", ErrReplayMismatch, err))
		}
		if err := emit(ctx, c, event.KindSideEffectRecorded, event.SideEffectRecorded{
			Name:  ndetNameRand,
			Value: raw,
		}); err != nil {
			panic(fmt.Sprintf("step.Random: replay emit: %v", err))
		}
		return v
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
// invoking fn and re-emits a matching SideEffectRecorded event so
// the replayed chain stays aligned. fn errors are propagated unrecorded
// (replay re-runs fn). T must be CBOR-serialisable. Panics on missing
// ctx or replay decode failure.
func SideEffect[T any](ctx context.Context, name string, fn func() (T, error)) (T, error) {
	c := mustFrom(ctx, "SideEffect")
	if c.mode == ModeReplay {
		raw, err := peekReplaySideEffect(c, name)
		if err != nil {
			var zero T
			return zero, err
		}
		var out T
		if err := cborenc.Unmarshal(raw, &out); err != nil {
			panic(fmt.Sprintf("step.SideEffect(%q): replay decode: %v", name, err))
		}
		if emitErr := emit(ctx, c, event.KindSideEffectRecorded, event.SideEffectRecorded{
			Name:  name,
			Value: raw,
		}); emitErr != nil {
			return out, fmt.Errorf("step.SideEffect(%q): replay emit: %w", name, emitErr)
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

// peekReplaySideEffect reads the event at the next replay seq without
// advancing the chain cursor, asserts it is a SideEffectRecorded with
// Name == want, and returns its Value bytes. The subsequent emit() call
// is what advances nextSeq — that way replay walks the chain at the
// same cadence as live mode and every recorded event is re-appended to
// the sink log.
func peekReplaySideEffect(c *Context, want string) (cborenc.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := int(c.nextSeq - 1)
	if idx >= len(c.recorded) {
		return nil, fmt.Errorf("%w: replay stream exhausted at seq=%d (wanted SideEffectRecorded %q)", ErrReplayMismatch, c.nextSeq, want)
	}
	rec := c.recorded[idx]
	if rec.Kind != event.KindSideEffectRecorded {
		return nil, fmt.Errorf("%w: seq=%d expected SideEffectRecorded, got %s", ErrReplayMismatch, c.nextSeq, rec.Kind)
	}
	p, err := rec.AsSideEffectRecorded()
	if err != nil {
		return nil, fmt.Errorf("%w: seq=%d decode SideEffectRecorded: %v", ErrReplayMismatch, c.nextSeq, err)
	}
	if p.Name != want {
		return nil, fmt.Errorf("%w: seq=%d expected name %q, got %q", ErrReplayMismatch, c.nextSeq, want, p.Name)
	}
	return p.Value, nil
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
