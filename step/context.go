package step

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
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

	// mode, recorded, and replayIdx drive the non-determinism helpers
	// (step.Now / step.Random / step.SideEffect). Under ModeReplay the
	// helpers scan recorded[replayIdx:] for the next SideEffectRecorded
	// instead of running their effect.
	mode      Mode
	recorded  []event.Event
	replayIdx int
	clockFn   func() time.Time

	// maxParallelTools caps concurrent tool execution for CallTools.
	// 0 means use DefaultMaxParallelTools.
	maxParallelTools int

	mu       sync.Mutex
	nextSeq  uint64
	prevHash []byte
}

// NewContext returns a Context primed to emit the first event of a run
// (seq=1, prevHash=nil). cfg.Log and cfg.RunID are required; Provider
// and Tools are optional at construction and only checked lazily by
// LLMCall and CallTool respectively.
func NewContext(cfg Config) *Context {
	if cfg.Log == nil {
		panic("step.NewContext: cfg.Log is nil")
	}
	if cfg.RunID == "" {
		panic("step.NewContext: cfg.RunID is empty")
	}
	if cfg.Mode == ModeReplay && cfg.Recorded == nil {
		panic("step.NewContext: ModeReplay requires cfg.Recorded")
	}
	clockFn := cfg.ClockFn
	if clockFn == nil {
		clockFn = time.Now
	}
	return &Context{
		log:      cfg.Log,
		runID:    cfg.RunID,
		provider: cfg.Provider,
		tools:    cfg.Tools,
		budget:   cfg.Budget,
		mode:             cfg.Mode,
		recorded:         cfg.Recorded,
		clockFn:          clockFn,
		maxParallelTools: cfg.MaxParallelTools,
		nextSeq:          1,
	}
}

// RunID returns the run identifier associated with the context. Useful
// for tools / tests that need to correlate external state with the
// current run.
func (c *Context) RunID() string { return c.runID }

// prov returns the provider configured on this Context, or nil if none.
func (c *Context) prov() provider.Provider { return c.provider }

// registry returns the tool registry, or nil if none.
func (c *Context) registry() *Registry { return c.tools }

// budgetCfg returns the pre-call budget configuration.
func (c *Context) budgetCfg() BudgetConfig { return c.budget }

// emit builds the full Event envelope for payload, advances the chain
// cursor, and appends to the log. Safe for concurrent use.
//
// kind must match the payload struct type; callers pass both so the
// encoder stays generic without reflection.
//
// Under ModeReplay, emit compares the would-be event against the
// recorded event at the same seq: Kind + Payload must match, and the
// recorded Timestamp is reused so the chain hash sequence is
// byte-identical to the original run. A mismatch returns an error
// wrapping ErrReplayMismatch; the verifier (replay.Verify) translates
// that into starling.ErrNonDeterminism.
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
	if err := c.log.Append(context.WithoutCancel(ctx), c.runID, ev); err != nil {
		return err
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
