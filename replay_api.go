package starling

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/replay"
)

// ReplayOption tunes Replay behavior. See WithForceProvider.
type ReplayOption = replay.Option

// WithForceProvider disables Replay's provider/model identity check.
// By default Replay refuses to run when the agent's
// Provider.ID/APIVersion/Config.Model differ from the values
// recorded in the log's RunStarted event; this catches the common
// "wrong agent factory" mistake before any turn executes. Pass this
// option only when the divergence is intentional.
func WithForceProvider() ReplayOption { return replay.WithForceProvider() }

// Replay re-executes runID against a and verifies the reproduced
// event sequence matches the recorded log byte-for-byte. a must be
// configured identically to the original run (same Tools, same
// Config); Provider and Log are overridden with replay equivalents
// and the caller's log stays untouched.
//
// Returns ErrNonDeterminism (wrapped) on divergence;
// ErrProviderModelMismatch when a's Provider.ID/APIVersion/Config.Model
// disagree with the recording (override with WithForceProvider);
// other errors (log-read, tool execution) surface verbatim.
func Replay(ctx context.Context, log eventlog.EventLog, runID string, a *Agent, opts ...ReplayOption) error {
	if err := replay.Verify(ctx, log, runID, a, opts...); err != nil {
		if errors.Is(err, replay.ErrNonDeterminism) {
			return fmt.Errorf("%w: %v", ErrNonDeterminism, err)
		}
		return err
	}
	return nil
}
