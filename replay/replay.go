// Package replay runs a recorded event log back through the agent loop
// and verifies the reproduced event sequence matches the log.
//
// Verify re-executes the run against a fresh in-memory log using a
// replay-mode provider that reconstructs streams from the recorded
// assistant messages. Every event the re-executed agent tries to emit
// is compared against the recorded event at the matching seq
// (byte-for-byte on Payload; Timestamps come from the recording so the
// chain hash sequence is identical).
//
// Tools are re-executed live; their outputs are compared against the
// recorded ToolCallCompleted.Result via the same emit-compare path. A
// tool that now returns different bytes surfaces as a divergence.
//
// Any mismatch returns an error that wraps ErrNonDeterminism; callers
// route on it with errors.Is.
package replay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/step"
)

// Divergence is a structured view of a replay mismatch. Callers extract
// it from a Verify error with errors.As.
type Divergence struct {
	RunID        string
	Seq          uint64
	Kind         event.Kind
	ExpectedKind event.Kind
	Class        step.MismatchClass
	Reason       string
}

func (d *Divergence) Error() string { return d.Reason }
func (d *Divergence) Unwrap() error { return ErrNonDeterminism }

// LogAttrs returns the structured attributes describing d, suitable
// for slog handlers that want to filter on individual fields.
func (d *Divergence) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("run_id", d.RunID),
		slog.Uint64("seq", d.Seq),
		slog.String("kind", d.Kind.String()),
		slog.String("expected_kind", d.ExpectedKind.String()),
		slog.String("class", string(d.Class)),
		slog.String("reason", d.Reason),
	}
}

// asDivergence extracts a *step.MismatchError from err and lifts it
// into a *Divergence stamped with runID. Returns nil if err is not a
// mismatch.
func asDivergence(runID string, err error) *Divergence {
	var m *step.MismatchError
	if !errors.As(err, &m) {
		return nil
	}
	return &Divergence{
		RunID:        runID,
		Seq:          m.Seq,
		Kind:         m.Kind,
		ExpectedKind: m.ExpectedKind,
		Class:        m.Class,
		Reason:       m.Reason,
	}
}

// ErrNonDeterminism is returned by Verify when the re-executed run
// diverges from the recording. Callers route on it with errors.Is.
// The root starling package re-exports this as starling.ErrNonDeterminism.
var ErrNonDeterminism = errors.New("replay: non-determinism detected")

// Agent is the subset of *starling.Agent fields Verify needs. The
// root starling package calls Verify with a concrete *Agent via its
// own Replay wrapper; this interface exists so package replay doesn't
// need to import starling (which would cycle).
type Agent interface {
	// RunReplay executes the agent in replay mode against recorded.
	// The RunID is taken from recorded[0].RunID; the provider is
	// replaced with a replay provider; the log is a fresh in-memory
	// log. Returns nil on clean replay, an ErrReplayMismatch-wrapped
	// error on divergence, or any run error encountered (tool
	// failures, etc.).
	RunReplay(ctx context.Context, recorded []event.Event) error
}

// Verify loads the run identified by runID from log and re-executes
// it against a. Returns nil iff every re-emitted event byte-matches
// the recording; ErrNonDeterminism-wrapped otherwise.
func Verify(ctx context.Context, log eventlog.EventLog, runID string, a Agent) error {
	recorded, err := log.Read(ctx, runID)
	if err != nil {
		return fmt.Errorf("replay: read log: %w", err)
	}
	if len(recorded) == 0 {
		return fmt.Errorf("replay: run %q not found", runID)
	}
	if err := a.RunReplay(ctx, recorded); err != nil {
		if d := asDivergence(runID, err); d != nil {
			slog.Default().LogAttrs(ctx, slog.LevelError, "replay divergence", d.LogAttrs()...)
			return fmt.Errorf("%w: %w", ErrNonDeterminism, d)
		}
		return err
	}
	return nil
}

// NewProvider exposes the replay-mode provider for callers (the root
// starling package) that need to construct their own run pipeline.
// info is used for the provider.Info() response; recorded is the event
// stream that seeds the reconstructed streams.
func NewProvider(info provider.Info, recorded []event.Event) (provider.Provider, error) {
	return newReplayProvider(info, recorded)
}
