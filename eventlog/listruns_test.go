package eventlog_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

// terminalNext is a chainBuilder helper that emits a RunCompleted-shaped
// event so tests can exercise TerminalKind without first-class support
// in the existing chainBuilder.
func (cb *chainBuilder) nextKind(t *testing.T, runID string, ts int64, kind event.Kind, payload []byte) event.Event {
	t.Helper()
	cb.seq++
	ev := event.Event{
		RunID:     runID,
		Seq:       cb.seq,
		PrevHash:  cb.prevHash,
		Timestamp: ts,
		Kind:      kind,
		Payload:   payload,
	}
	encoded, err := event.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cb.prevHash = event.Hash(encoded)
	return ev
}

// listRunsBackends parameterises every test in this file across both
// backends so behaviour is provably identical.
func listRunsBackends(t *testing.T) []struct {
	name string
	open func(t *testing.T) eventlog.EventLog
} {
	t.Helper()
	dir := t.TempDir()
	return []struct {
		name string
		open func(t *testing.T) eventlog.EventLog
	}{
		{
			name: "memory",
			open: func(*testing.T) eventlog.EventLog { return eventlog.NewInMemory() },
		},
		{
			name: "sqlite",
			open: func(t *testing.T) eventlog.EventLog {
				name := strings.ReplaceAll(t.Name(), "/", "_") + ".db"
				log, err := eventlog.NewSQLite(filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("NewSQLite: %v", err)
				}
				return log
			},
		},
	}
}

func TestListRuns_Empty(t *testing.T) {
	for _, bk := range listRunsBackends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			defer log.Close()
			runs, err := log.ListRuns(context.Background())
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(runs) != 0 {
				t.Fatalf("got %d runs, want 0", len(runs))
			}
		})
	}
}

func TestListRuns_PerRunSummary(t *testing.T) {
	completedPayload, err := event.EncodePayload(event.RunCompleted{
		FinalText: "ok", TurnCount: 1,
	})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}

	for _, bk := range listRunsBackends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			defer log.Close()

			ctx := context.Background()

			// Two runs: r1 ends with RunCompleted (terminal), r2
			// stops after a UserMessageAppended (still in-progress).
			cb1 := &chainBuilder{}
			if err := log.Append(ctx, "r1", cb1.next(t, "r1", "hi")); err != nil {
				t.Fatalf("append r1.1: %v", err)
			}
			if err := log.Append(ctx, "r1", cb1.nextKind(t, "r1", 5_000_000, event.KindRunCompleted, completedPayload)); err != nil {
				t.Fatalf("append r1.2: %v", err)
			}

			cb2 := &chainBuilder{}
			if err := log.Append(ctx, "r2", cb2.next(t, "r2", "hi")); err != nil {
				t.Fatalf("append r2.1: %v", err)
			}

			runs, err := log.ListRuns(ctx)
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(runs) != 2 {
				t.Fatalf("got %d runs, want 2: %+v", len(runs), runs)
			}

			// Index by RunID for deterministic assertions.
			byID := map[string]eventlog.RunSummary{}
			for _, r := range runs {
				byID[r.RunID] = r
			}

			r1 := byID["r1"]
			if r1.LastSeq != 2 {
				t.Errorf("r1.LastSeq = %d, want 2", r1.LastSeq)
			}
			if r1.TerminalKind != event.KindRunCompleted {
				t.Errorf("r1.TerminalKind = %s, want RunCompleted", r1.TerminalKind)
			}
			if !r1.TerminalKind.IsTerminal() {
				t.Errorf("r1.TerminalKind.IsTerminal() = false, want true")
			}

			r2 := byID["r2"]
			if r2.LastSeq != 1 {
				t.Errorf("r2.LastSeq = %d, want 1", r2.LastSeq)
			}
			if r2.TerminalKind != event.KindUserMessageAppended {
				t.Errorf("r2.TerminalKind = %s, want UserMessageAppended", r2.TerminalKind)
			}
			if r2.TerminalKind.IsTerminal() {
				t.Errorf("r2.TerminalKind.IsTerminal() = true, want false (still in-progress)")
			}

			// StartedAt should reflect the first event's timestamp.
			wantR1Start := time.Unix(0, 1_000_000)
			if !r1.StartedAt.Equal(wantR1Start) {
				t.Errorf("r1.StartedAt = %v, want %v", r1.StartedAt, wantR1Start)
			}
		})
	}
}

func TestListRuns_NewestFirst(t *testing.T) {
	for _, bk := range listRunsBackends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			defer log.Close()

			ctx := context.Background()

			// Append three runs whose first-event timestamps put them
			// in the order older=r-old, middle=r-mid, newest=r-new.
			for _, run := range []struct {
				id string
				ts int64
			}{
				{"r-old", 1_000_000},
				{"r-new", 9_000_000},
				{"r-mid", 5_000_000},
			} {
				cb := &chainBuilder{}
				ev := cb.next(t, run.id, "x")
				ev.Timestamp = run.ts
				// Re-marshal and rehash since we mutated the timestamp.
				encoded, err := event.Marshal(ev)
				if err != nil {
					t.Fatalf("Marshal: %v", err)
				}
				cb.prevHash = event.Hash(encoded)
				if err := log.Append(ctx, run.id, ev); err != nil {
					t.Fatalf("append %s: %v", run.id, err)
				}
			}

			runs, err := log.ListRuns(ctx)
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(runs) != 3 {
				t.Fatalf("got %d runs, want 3", len(runs))
			}
			want := []string{"r-new", "r-mid", "r-old"}
			for i, r := range runs {
				if r.RunID != want[i] {
					t.Errorf("runs[%d].RunID = %s, want %s", i, r.RunID, want[i])
				}
			}
		})
	}
}

func TestListRuns_AfterClose(t *testing.T) {
	for _, bk := range listRunsBackends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			log.Close()
			if _, err := log.ListRuns(context.Background()); err == nil {
				t.Fatal("ListRuns after Close: want error, got nil")
			}
		})
	}
}
