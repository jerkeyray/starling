package starlingtest

import (
	"context"
	"errors"
	"testing"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
)

// AssertReplayMatches replays runID against agent and fails the test
// if replay does not complete cleanly. The agent must be configured
// with the same Tools, Config, and Provider Info as the original run.
//
// Use this in regression tests to catch behavior drift: a recorded
// log + an agent factory should keep replaying clean across refactors.
func AssertReplayMatches(t *testing.T, log eventlog.EventLog, runID string, agent *starling.Agent) {
	t.Helper()
	if err := starling.Replay(context.Background(), log, runID, agent); err != nil {
		t.Fatalf("starlingtest: AssertReplayMatches: %v", err)
	}
}

// AssertReplayDiverges replays runID against agent and fails the test
// unless the replay returns ErrNonDeterminism.
//
// Use this to confirm a deliberate divergence — e.g. a tool whose
// output now differs from the recorded result — surfaces as the
// replay error consumers are expected to detect.
func AssertReplayDiverges(t *testing.T, log eventlog.EventLog, runID string, agent *starling.Agent) {
	t.Helper()
	err := starling.Replay(context.Background(), log, runID, agent)
	if !errors.Is(err, starling.ErrNonDeterminism) {
		t.Fatalf("starlingtest: AssertReplayDiverges: err = %v, want ErrNonDeterminism", err)
	}
}
