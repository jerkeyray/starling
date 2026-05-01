// Package replay re-runs a recorded event log through the agent loop
// and verifies the reproduced events match the recording byte-for-byte.
// Streams are reconstructed from recorded assistant messages; tools
// are re-executed live and their outputs compared against the recorded
// ToolCallCompleted.Result. Mismatches return an error wrapping
// ErrNonDeterminism.
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

// ErrNonDeterminism is returned by Verify when the re-executed run
// diverges from the recording. Callers route on it with errors.Is.
// The root starling package re-exports this as starling.ErrNonDeterminism.
var ErrNonDeterminism = errors.New("replay: non-determinism detected")

// ErrProviderModelMismatch is returned by Verify when the replay
// agent's Provider.ID, APIVersion, or Config.Model does not match
// what the recording's RunStarted event captured. Cross-provider or
// cross-model replay is almost always a configuration mistake and
// will produce a non-deterministic run; the check trips before any
// turn executes so the failure mode is explicit.
//
// Callers that intentionally want to replay a recording against a
// different provider (e.g. comparing OpenAI and Anthropic on the same
// log) can pass WithForceProvider to bypass the check.
var ErrProviderModelMismatch = errors.New("replay: provider/model mismatch with recording")

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

// asDivergence lifts a *step.MismatchError out of err into a
// *Divergence stamped with runID. Returns nil when err is not a
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

// ProviderInspector is the optional interface a replay Agent
// implements so Verify can compare its provider/model identity
// against the recording's RunStarted event. *starling.Agent
// implements this in the runtime; tests that pass a bare RunReplay
// implementation can opt out by not implementing it (the check is
// then skipped).
type ProviderInspector interface {
	ReplayProviderInfo() (providerID, apiVersion, modelID string)
}

// Option tunes Verify behavior.
type Option func(*config)

type config struct {
	force bool
}

// WithForceProvider disables the provider/model identity check that
// normally rejects replays whose recording came from a different
// Provider.ID, APIVersion, or Config.Model. Use this only when the
// divergence is intentional (e.g. comparing two providers on the
// same input log).
func WithForceProvider() Option { return func(c *config) { c.force = true } }

// Verify loads the run identified by runID from log and re-executes
// it against a. Returns nil iff every re-emitted event byte-matches
// the recording; ErrNonDeterminism-wrapped otherwise.
func Verify(ctx context.Context, log eventlog.EventLog, runID string, a Agent, opts ...Option) error {
	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}
	recorded, err := log.Read(ctx, runID)
	if err != nil {
		return fmt.Errorf("replay: read log: %w", err)
	}
	if len(recorded) == 0 {
		return fmt.Errorf("replay: run %q not found", runID)
	}
	if !cfg.force {
		if err := checkProviderMatch(a, recorded[0]); err != nil {
			return err
		}
	}
	if err := a.RunReplay(ctx, recorded); err != nil {
		if d := asDivergence(runID, err); d != nil {
			// Replay divergence is a safety-critical signal and is
			// always logged via slog.Default(), regardless of
			// Config.Logger. Documented in Config.Logger godoc.
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

// checkProviderMatch compares the live agent's provider/model against
// the RunStarted event of the recording. Skipped when the agent does
// not implement ProviderInspector (e.g. test fakes).
func checkProviderMatch(a Agent, first event.Event) error {
	insp, ok := a.(ProviderInspector)
	if !ok {
		return nil
	}
	if first.Kind != event.KindRunStarted {
		return nil
	}
	rs, err := first.AsRunStarted()
	if err != nil {
		return nil
	}
	gotProv, gotAPI, gotModel := insp.ReplayProviderInfo()
	if rs.ProviderID != gotProv || rs.APIVersion != gotAPI || rs.ModelID != gotModel {
		return fmt.Errorf("%w: recording=%s/%s/%s, agent=%s/%s/%s (use WithForceProvider to override)",
			ErrProviderModelMismatch,
			rs.ProviderID, rs.APIVersion, rs.ModelID,
			gotProv, gotAPI, gotModel,
		)
	}
	return nil
}
