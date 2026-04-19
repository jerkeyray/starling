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
	return &Context{
		log:      cfg.Log,
		runID:    cfg.RunID,
		provider: cfg.Provider,
		tools:    cfg.Tools,
		budget:   cfg.Budget,
		nextSeq:  1,
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
func emit[T any](ctx context.Context, c *Context, kind event.Kind, payload T) error {
	encoded, err := event.EncodePayload(payload)
	if err != nil {
		return fmt.Errorf("step: encode %s payload: %w", kind, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	ev := event.Event{
		RunID:     c.runID,
		Seq:       c.nextSeq,
		PrevHash:  c.prevHash,
		Timestamp: time.Now().UnixNano(),
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
