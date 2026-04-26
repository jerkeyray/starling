package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/replay"
)

// TestReplayRegression_TriageRun records a fresh run end-to-end and
// then replays it against the same agent factory. Any drift in the
// canned conversation, tool implementations, or step.SideEffect
// recording surfaces as a non-nil error from replay.Verify.
func TestReplayRegression_TriageRun(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "triage.db")

	a, err := buildAgent(context.Background(), buildOpts{
		dbPath:    dbPath,
		useCanned: true,
		notify:    func(_, _ string) error { return nil },
	})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	defer a.Log.(interface{ Close() error }).Close()

	res, err := a.Run(context.Background(), "checkout-api error_rate spiked at 14:00 UTC; triage and escalate if necessary.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TerminalKind != event.KindRunCompleted {
		t.Fatalf("TerminalKind = %s, want RunCompleted", res.TerminalKind)
	}
	if res.TurnCount < 2 {
		t.Fatalf("TurnCount = %d, want >= 2", res.TurnCount)
	}
	if vErr := loadValidate(a.Log, res.RunID); vErr != nil {
		t.Fatalf("Validate: %v", vErr)
	}
	if testing.Verbose() {
		evs, _ := a.Log.Read(context.Background(), res.RunID)
		for _, ev := range evs {
			t.Logf("seq=%d kind=%s payload=%x", ev.Seq, ev.Kind, ev.Payload)
		}
	}

	// Open a fresh handle for replay so we exercise the read path.
	roLog, err := eventlog.NewSQLite(dbPath, eventlog.WithReadOnly())
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer roLog.Close()

	factory := replay.Factory(func(ctx context.Context) (replay.Agent, error) {
		// Different DB path so the replay agent's own log isn't the
		// one we're replaying against.
		return buildAgent(ctx, buildOpts{
			dbPath:    filepath.Join(t.TempDir(), "replay.db"),
			useCanned: true,
			notify:    func(_, _ string) error { return nil },
		})
	})
	verifyAgent, err := factory(context.Background())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if err := replay.Verify(context.Background(), roLog, res.RunID, verifyAgent); err != nil {
		t.Fatalf("replay.Verify: %v", err)
	}
}
