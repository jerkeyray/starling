package starling

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/replay"
)

// Replay re-executes runID against a and verifies the reproduced
// event sequence matches the recorded log byte-for-byte. a must be
// configured identically to the original run (same Tools, same
// Config); Provider and Log are overridden with replay equivalents
// and the caller's log stays untouched.
//
// Returns ErrNonDeterminism (wrapped) on divergence; other errors
// (log-read, tool execution) surface verbatim.
func Replay(ctx context.Context, log eventlog.EventLog, runID string, a *Agent) error {
	if err := replay.Verify(ctx, log, runID, a); err != nil {
		if errors.Is(err, replay.ErrNonDeterminism) {
			return fmt.Errorf("%w: %v", ErrNonDeterminism, err)
		}
		return err
	}
	return nil
}
