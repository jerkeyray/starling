package step

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/internal/obs"
	"github.com/jerkeyray/starling/provider"
)

// Context is the opaque per-run state attached to every stdlib
// context.Context inside an agent run. It owns the hash-chain cursor
// (next seq + previous event hash) so that any step-level emitter sees
// a consistent view even under concurrent Now/Random/SideEffect calls.
//
// Callers extract it with From and pass it into the non-deterministic
// helpers in this package. Construction is normally handled by the
// agent loop (the root starling package); advanced users wiring their
// own loops may call NewContext directly.
type Context struct {
	log      eventlog.EventLog
	runID    string
	provider provider.Provider
	tools    *Registry
	budget   BudgetConfig

	// Replay mode: helpers (Now/Random/SideEffect) scan
	// recorded[replayIdx:] instead of running their effect.
	mode      Mode
	recorded  []event.Event
	replayIdx int
	clockFn   func() time.Time

	// Caps concurrent tool execution; 0 → DefaultMaxParallelTools.
	maxParallelTools int

	// Never nil — NewContext substitutes a discard logger.
	logger *slog.Logger

	// May be nil; emit sites must tolerate a nil sink.
	metrics MetricsSink

	mu       sync.Mutex
	nextSeq  uint64
	prevHash []byte
}

// ErrInvalidConfig is returned by NewContext when cfg lacks a
// required field (Log, RunID) or combines ModeReplay without
// Recorded.
var ErrInvalidConfig = fmt.Errorf("step: invalid Config")

// NewContext returns a Context primed to emit the first event
// (seq=1, prevHash=nil). Log and RunID are required; Provider and
// Tools are checked lazily by LLMCall and CallTool.
func NewContext(cfg Config) (*Context, error) {
	if cfg.Log == nil {
		return nil, fmt.Errorf("%w: cfg.Log is nil", ErrInvalidConfig)
	}
	if cfg.RunID == "" {
		return nil, fmt.Errorf("%w: cfg.RunID is empty", ErrInvalidConfig)
	}
	if cfg.Mode == ModeReplay && cfg.Recorded == nil {
		return nil, fmt.Errorf("%w: ModeReplay requires cfg.Recorded", ErrInvalidConfig)
	}
	clockFn := cfg.ClockFn
	if clockFn == nil {
		clockFn = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = obs.Discard()
	}
	nextSeq := uint64(1)
	var prevHash []byte
	if cfg.ResumeFromSeq > 0 {
		nextSeq = cfg.ResumeFromSeq + 1
		prevHash = append(prevHash, cfg.ResumeFromPrevHash...)
	}
	return &Context{
		log:              cfg.Log,
		runID:            cfg.RunID,
		provider:         cfg.Provider,
		tools:            cfg.Tools,
		budget:           cfg.Budget,
		mode:             cfg.Mode,
		recorded:         cfg.Recorded,
		clockFn:          clockFn,
		maxParallelTools: cfg.MaxParallelTools,
		logger:           logger,
		metrics:          cfg.Metrics,
		nextSeq:          nextSeq,
		prevHash:         prevHash,
	}, nil
}

// MustNewContext is NewContext without the error return: it panics on
// invalid config. Useful in test setup and in the agent loop where a
// bad config is a programmer bug. Production callers should prefer
// NewContext.
func MustNewContext(cfg Config) *Context {
	c, err := NewContext(cfg)
	if err != nil {
		panic(err)
	}
	return c
}

// RunID returns the run identifier associated with the context. Useful
// for tools / tests that need to correlate external state with the
// current run.
func (c *Context) RunID() string { return c.runID }

// Logger returns the slog.Logger bound to this run. Never nil: returns
// a discard logger when the agent was built without one. Intended for
// the agent loop and downstream tool implementations that want to
// participate in the run's structured trace.
func (c *Context) Logger() *slog.Logger { return c.logger }

// Metrics returns the MetricsSink this Context records to, or nil
// when metrics are disabled. Call sites should treat nil as a
// no-op rather than guarding every method call.
func (c *Context) Metrics() MetricsSink { return c.metrics }

// prov returns the provider configured on this Context, or nil if none.
func (c *Context) prov() provider.Provider { return c.provider }

// registry returns the tool registry, or nil if none.
func (c *Context) registry() *Registry { return c.tools }

// budgetCfg returns the pre-call budget configuration.
func (c *Context) budgetCfg() BudgetConfig { return c.budget }

// emit builds the Event envelope, advances the chain cursor, and
// appends to the log. Safe for concurrent use. Under ModeReplay,
// compares against recorded[seq]: Kind+Payload must match, and the
// recorded Timestamp is reused for byte-identical chain hashes. A
// mismatch returns ErrReplayMismatch.
func emit[T any](ctx context.Context, c *Context, kind event.Kind, payload T) error {
	encoded, err := event.EncodePayload(payload)
	if err != nil {
		return fmt.Errorf("step: encode %s payload: %w", kind, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var ts int64
	if c.mode == ModeReplay {
		idx := int(c.nextSeq - 1)
		if idx >= len(c.recorded) {
			return fmt.Errorf("%w: replay stream exhausted at seq=%d (kind=%s)", ErrReplayMismatch, c.nextSeq, kind)
		}
		rec := c.recorded[idx]
		if rec.Kind != kind {
			return fmt.Errorf("%w: seq=%d expected kind %s, got %s", ErrReplayMismatch, c.nextSeq, rec.Kind, kind)
		}
		if !bytesEqual(rec.Payload, encoded) {
			return fmt.Errorf("%w: seq=%d payload mismatch (kind=%s)", ErrReplayMismatch, c.nextSeq, kind)
		}
		ts = rec.Timestamp
	} else {
		ts = c.clockFn().UnixNano()
	}

	ev := event.Event{
		RunID:     c.runID,
		Seq:       c.nextSeq,
		PrevHash:  c.prevHash,
		Timestamp: ts,
		Kind:      kind,
		Payload:   encoded,
	}
	marshaled, err := event.Marshal(ev)
	if err != nil {
		return fmt.Errorf("step: marshal event: %w", err)
	}
	// Use an uncancellable ctx for the log write: once we've decided to
	// emit, a downstream ctx cancellation must not drop the audit trail.
	// In particular, tool-failure events for cancelled tools would
	// otherwise be silently dropped, leaving the chain terminated at
	// the Scheduled event with no matching outcome.
	start := time.Now()
	appendErr := c.log.Append(context.WithoutCancel(ctx), c.runID, ev)
	if c.metrics != nil {
		status := "ok"
		if appendErr != nil {
			status = "error"
		}
		c.metrics.ObserveEventlogAppend(kind.String(), status, time.Since(start))
	}
	if appendErr != nil {
		return appendErr
	}
	c.nextSeq++
	c.prevHash = event.Hash(marshaled)
	return nil
}

// mintTurnID returns the TurnID the next TurnStarted event should
// carry. In live mode it generates a fresh ULID; in replay mode it
// reads the TurnID from the recorded TurnStarted at the current seq
// so the re-emitted payload byte-matches the recording. Returns an
// ErrReplayMismatch-wrapped error if the recorded event at that seq
// is not a TurnStarted or fails to decode.
func (c *Context) mintTurnID() (string, error) {
	c.mu.Lock()
	idx := int(c.nextSeq - 1)
	mode := c.mode
	var rec event.Event
	if mode == ModeReplay && idx < len(c.recorded) {
		rec = c.recorded[idx]
	}
	exhausted := mode == ModeReplay && idx >= len(c.recorded)
	c.mu.Unlock()

	if mode != ModeReplay {
		return newULID(), nil
	}
	if exhausted {
		return "", fmt.Errorf("%w: replay stream exhausted minting TurnID at seq=%d", ErrReplayMismatch, idx+1)
	}
	if rec.Kind != event.KindTurnStarted {
		return "", fmt.Errorf("%w: seq=%d expected TurnStarted, got %s", ErrReplayMismatch, rec.Seq, rec.Kind)
	}
	ts, err := rec.AsTurnStarted()
	if err != nil {
		return "", fmt.Errorf("%w: seq=%d decode TurnStarted: %v", ErrReplayMismatch, rec.Seq, err)
	}
	return ts.TurnID, nil
}

// ReplayDurationMs returns the duration_ms value from the event
// recorded at the next seq position during replay; in live mode it
// returns fallback unchanged. Wall-clock durations are inherently
// non-deterministic (live and replay take different amounts of time,
// especially under the race detector), so any event carrying a
// DurationMs field must route its value through this helper before
// emission to keep emit-compare happy.
//
// Applies to ToolCallCompleted, ToolCallFailed, RunCompleted,
// RunCancelled, and RunFailed — every event whose payload includes a
// `duration_ms` CBOR field. The helper decodes only that one field, so
// it's robust to unrelated schema additions on those payload types.
func ReplayDurationMs(ctx context.Context, fallback int64) int64 {
	c := mustFrom(ctx, "ReplayDurationMs")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != ModeReplay {
		return fallback
	}
	idx := int(c.nextSeq - 1)
	if idx >= len(c.recorded) {
		return fallback
	}
	var tmp struct {
		DurationMs int64 `cbor:"duration_ms"`
	}
	if err := cborenc.Unmarshal(c.recorded[idx].Payload, &tmp); err != nil {
		return fallback
	}
	return tmp.DurationMs
}

// bytesEqual avoids importing "bytes" just for one comparison.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Emit writes a typed event payload into the run's log, advancing the
// hash chain under c.mu. Intended for use by the agent loop (root
// starling package) which emits RunStarted and terminal events itself
// — step's own helpers use the unexported emit directly.
//
// kind must match the payload type. Safe for concurrent use.
func Emit[T any](ctx context.Context, c *Context, kind event.Kind, payload T) error {
	return emit(ctx, c, kind, payload)
}

// ctxKey is a private empty type used as the context-value key. Using a
// struct{} type (rather than a string) avoids collisions with any other
// package's context values.
type ctxKey struct{}

// WithContext returns a derived context.Context carrying c.
func WithContext(parent context.Context, c *Context) context.Context {
	return context.WithValue(parent, ctxKey{}, c)
}

// From extracts the Context previously attached via WithContext. When no
// step.Context has been attached the second return is false and the
// first is nil — callers that must emit events should treat this as a
// programmer error.
func From(ctx context.Context) (*Context, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Context)
	return c, ok
}

// mustFrom is the internal version used by the ndet helpers. Missing
// Context is a programmer bug (value produced without emission =
// non-replayable run) so we panic rather than quietly degrade.
func mustFrom(ctx context.Context, fn string) *Context {
	c, ok := From(ctx)
	if !ok {
		panic(fmt.Sprintf("step.%s: no step.Context attached to ctx (call only from inside an agent run)", fn))
	}
	return c
}
