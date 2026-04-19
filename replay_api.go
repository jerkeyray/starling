package starling

import (
	"context"
	"errors"
	"fmt"

	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/replay"
)

// Replay re-executes the run identified by runID against a and
// verifies the reproduced event sequence matches the recorded log
// byte-for-byte. Intended use: after an agent crash or process exit,
// call Replay with the same Agent configuration to prove the run is
// reproducible.
//
// Returns nil on clean replay. Returns an error wrapping
// ErrNonDeterminism if any emitted event diverges from its recording
// (tool output drift, code path changes that affect event payloads,
// etc.). Other errors surface verbatim (log-read failures, tool
// execution failures, etc.).
//
// a should be configured identically to the original run (same Tools,
// same Config). Replay overrides Provider (replaced with a recording-
// driven replay provider) and Log (replaced with a scratch in-memory
// log); the caller's Log is read-only here and remains untouched.
func Replay(ctx context.Context, log eventlog.EventLog, runID string, a *Agent) error {
	if err := replay.Verify(ctx, log, runID, a); err != nil {
		if errors.Is(err, replay.ErrNonDeterminism) {
			return fmt.Errorf("%w: %v", ErrNonDeterminism, err)
		}
		return err
	}
	return nil
}
