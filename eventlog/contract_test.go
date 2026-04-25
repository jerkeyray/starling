package eventlog_test

// Contract tests — every eventlog backend must pass every test in this
// file. Running the same body against multiple backends keeps them
// provably interchangeable: a caller switching from NewInMemory to
// NewSQLite (or, later, NewPostgres) must not change any observable
// behaviour covered here.
//
// Backend-specific behaviour (file persistence, WAL semantics, advisory
// locks, slow-consumer drops) lives in the per-backend test file.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/merkle"
)

// sealChain appends a RunCompleted terminal with a correct MerkleRoot
// over the existing events so eventlog.Validate passes. Returns the
// full sealed slice. Used by tests that build a raw chain and want
// to exercise Validate on the result.
//
// The terminal's Seq and PrevHash are derived from the last element
// of existing, not from a chainBuilder — under contended-append
// workloads the shared chainBuilder can drift below the DB's real
// tail, and the just-read slice is the only authoritative view.
func sealChain(t *testing.T, ctx context.Context, log eventlog.EventLog, runID string, existing []event.Event) []event.Event {
	t.Helper()
	if len(existing) == 0 {
		t.Fatalf("sealChain: existing is empty")
	}
	last := existing[len(existing)-1]
	lastEnc, err := event.Marshal(last)
	if err != nil {
		t.Fatalf("Marshal tail: %v", err)
	}
	leaves, err := merkle.EventHashes(existing)
	if err != nil {
		t.Fatalf("EventHashes: %v", err)
	}
	root := merkle.Root(leaves)
	payload, err := event.EncodePayload(event.RunCompleted{
		FinalText: "ok", TurnCount: 0, MerkleRoot: root,
	})
	if err != nil {
		t.Fatalf("EncodePayload RunCompleted: %v", err)
	}
	term := event.Event{
		RunID:     runID,
		Seq:       last.Seq + 1,
		PrevHash:  event.Hash(lastEnc),
		Timestamp: int64(last.Seq+1) * 1_000_000,
		Kind:      event.KindRunCompleted,
		Payload:   payload,
	}
	if err := log.Append(ctx, runID, term); err != nil {
		t.Fatalf("append terminal: %v", err)
	}
	return append(existing, term)
}

// backend is one row of the contract matrix: a name (for t.Run
// subtests) and an open function that returns a fresh EventLog.
type backend struct {
	name string
	open func(t *testing.T) eventlog.EventLog
}

// backends returns every backend the contract suite covers. In commit 1
// this is memory + sqlite; commit 2 adds postgres behind an env gate.
//
// Each open func is expected to register its own Close on t.Cleanup;
// the shared suite doesn't double-close.
func backends(t *testing.T) []backend {
	t.Helper()
	dir := t.TempDir()
	bks := []backend{
		{
			name: "memory",
			open: func(t *testing.T) eventlog.EventLog {
				log := eventlog.NewInMemory()
				t.Cleanup(func() { _ = log.Close() })
				return log
			},
		},
		{
			name: "sqlite",
			open: func(t *testing.T) eventlog.EventLog {
				name := strings.ReplaceAll(t.Name(), "/", "_") + ".db"
				log, err := eventlog.NewSQLite(filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("NewSQLite: %v", err)
				}
				t.Cleanup(func() { _ = log.Close() })
				return log
			},
		},
	}
	if pgDSN() != "" {
		bks = append(bks, backend{
			name: "postgres",
			open: func(t *testing.T) eventlog.EventLog { return openPG(t) },
		})
	}
	return bks
}

// chainBuilder tracks per-run Seq/PrevHash state so tests don't
// reimplement chain arithmetic. Lives here (not in a backend-specific
// file) because every contract test uses it.
type chainBuilder struct {
	seq      uint64
	prevHash []byte
}

// next returns a fresh Event with valid Seq and PrevHash. The very
// first event for a chain is a RunStarted (so semantic validation
// passes); subsequent events are UserMessageAppended carrying content
// so events are distinct.
func (cb *chainBuilder) next(t *testing.T, runID, content string) event.Event {
	t.Helper()
	cb.seq++
	var (
		kind    event.Kind
		payload []byte
		err     error
	)
	if cb.seq == 1 {
		kind = event.KindRunStarted
		payload, err = event.EncodePayload(event.RunStarted{
			SchemaVersion: event.SchemaVersion,
			Goal:          content,
			ProviderID:    "test",
			ModelID:       "test",
		})
	} else {
		kind = event.KindUserMessageAppended
		payload, err = event.EncodePayload(event.UserMessageAppended{Content: content})
	}
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	ev := event.Event{
		RunID:     runID,
		Seq:       cb.seq,
		PrevHash:  cb.prevHash,
		Timestamp: int64(cb.seq) * 1_000_000,
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

// nextKind is the chainBuilder variant for minting a terminal-or-
// arbitrary-kind event. Used by tests that need TerminalKind control
// (ListRuns).
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

// -----------------------------------------------------------------------
// Append invariants
// -----------------------------------------------------------------------

func TestContract_Append_FirstEventMustStartAtSeq1(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			payload, _ := event.EncodePayload(event.UserMessageAppended{Content: "x"})
			ev := event.Event{RunID: "r1", Seq: 5, Kind: event.KindUserMessageAppended, Payload: payload}
			err := log.Append(context.Background(), "r1", ev)
			if !errors.Is(err, eventlog.ErrInvalidAppend) {
				t.Fatalf("want ErrInvalidAppend, got %v", err)
			}
		})
	}
}

func TestContract_Append_FirstEventMustHaveEmptyPrevHash(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			payload, _ := event.EncodePayload(event.UserMessageAppended{Content: "x"})
			ev := event.Event{
				RunID: "r1", Seq: 1, PrevHash: []byte{0x01},
				Kind: event.KindUserMessageAppended, Payload: payload,
			}
			err := log.Append(context.Background(), "r1", ev)
			if !errors.Is(err, eventlog.ErrInvalidAppend) {
				t.Fatalf("want ErrInvalidAppend, got %v", err)
			}
		})
	}
}

func TestContract_Append_SeqMustBeMonotonic(t *testing.T) {
	ctx := context.Background()
	for _, bk := range backends(t) {
		t.Run(bk.name+"/skip", func(t *testing.T) {
			log := bk.open(t)
			var cb chainBuilder
			if err := log.Append(ctx, "r1", cb.next(t, "r1", "a")); err != nil {
				t.Fatalf("first append: %v", err)
			}
			bad := cb.next(t, "r1", "b")
			bad.Seq = 3
			if err := log.Append(ctx, "r1", bad); !errors.Is(err, eventlog.ErrInvalidAppend) {
				t.Fatalf("want ErrInvalidAppend, got %v", err)
			}
		})
		t.Run(bk.name+"/replay", func(t *testing.T) {
			log := bk.open(t)
			var cb chainBuilder
			first := cb.next(t, "r1", "a")
			if err := log.Append(ctx, "r1", first); err != nil {
				t.Fatalf("first append: %v", err)
			}
			if err := log.Append(ctx, "r1", first); !errors.Is(err, eventlog.ErrInvalidAppend) {
				t.Fatalf("want ErrInvalidAppend, got %v", err)
			}
		})
	}
}

func TestContract_Append_PrevHashMustMatchPrevEvent(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			ctx := context.Background()
			var cb chainBuilder
			if err := log.Append(ctx, "r1", cb.next(t, "r1", "a")); err != nil {
				t.Fatalf("first append: %v", err)
			}
			bad := cb.next(t, "r1", "b")
			bad.PrevHash = make([]byte, event.HashSize)
			if err := log.Append(ctx, "r1", bad); !errors.Is(err, eventlog.ErrInvalidAppend) {
				t.Fatalf("want ErrInvalidAppend, got %v", err)
			}
		})
	}
}

func TestContract_Append_HappyPath(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			ctx := context.Background()
			var cb chainBuilder
			want := make([]event.Event, 5)
			for i := 0; i < 5; i++ {
				ev := cb.next(t, "r1", fmt.Sprintf("msg-%d", i))
				want[i] = ev
				if err := log.Append(ctx, "r1", ev); err != nil {
					t.Fatalf("append %d: %v", i, err)
				}
			}
			got, err := log.Read(ctx, "r1")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i].Seq != want[i].Seq {
					t.Errorf("events[%d].Seq = %d, want %d", i, got[i].Seq, want[i].Seq)
				}
				if !reflect.DeepEqual(got[i].PrevHash, want[i].PrevHash) {
					t.Errorf("events[%d].PrevHash mismatch", i)
				}
				if !reflect.DeepEqual(got[i].Payload, want[i].Payload) {
					t.Errorf("events[%d].Payload mismatch", i)
				}
				if got[i].Kind != want[i].Kind {
					t.Errorf("events[%d].Kind mismatch", i)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------
// Read
// -----------------------------------------------------------------------

func TestContract_Read_UnknownRunIDReturnsNil(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			got, err := log.Read(context.Background(), "never-seen")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got != nil {
				t.Fatalf("want nil slice, got %v", got)
			}
		})
	}
}

// TestContract_Read_ReturnsDefensiveCopy verifies that mutating the
// returned slice does not affect subsequent Read calls. For durable
// backends this is trivially true (each Read builds a fresh slice from
// rows); for the in-memory backend it's an explicit copy.
func TestContract_Read_ReturnsDefensiveCopy(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			ctx := context.Background()
			var cb chainBuilder
			for i := 0; i < 3; i++ {
				if err := log.Append(ctx, "r1", cb.next(t, "r1", "x")); err != nil {
					t.Fatalf("append: %v", err)
				}
			}
			snap, err := log.Read(ctx, "r1")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			snap[0].RunID = "TAMPERED"

			again, err := log.Read(ctx, "r1")
			if err != nil {
				t.Fatalf("Read again: %v", err)
			}
			if again[0].RunID != "r1" {
				t.Fatalf("internal state mutated via returned slice: RunID=%q", again[0].RunID)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Stream
// -----------------------------------------------------------------------

func TestContract_Stream_ReplayThenLive(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var cb chainBuilder
			historical := make([]event.Event, 3)
			for i := 0; i < 3; i++ {
				historical[i] = cb.next(t, "r1", fmt.Sprintf("hist-%d", i))
				if err := log.Append(ctx, "r1", historical[i]); err != nil {
					t.Fatalf("append hist %d: %v", i, err)
				}
			}

			ch, err := log.Stream(ctx, "r1")
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}

			// Drain historical.
			for i := 0; i < 3; i++ {
				select {
				case got := <-ch:
					if got.Seq != historical[i].Seq {
						t.Fatalf("hist[%d]: want Seq=%d, got %d", i, historical[i].Seq, got.Seq)
					}
				case <-time.After(2 * time.Second):
					t.Fatalf("timeout draining historical[%d]", i)
				}
			}

			// Append live; it should land within a few poll intervals.
			live := cb.next(t, "r1", "live-0")
			if err := log.Append(ctx, "r1", live); err != nil {
				t.Fatalf("append live: %v", err)
			}
			select {
			case got := <-ch:
				if got.Seq != live.Seq {
					t.Fatalf("live Seq = %d, want %d", got.Seq, live.Seq)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("live event not delivered within 2s")
			}
		})
	}
}

func TestContract_Stream_ContextCancelClosesChannel(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			ctx, cancel := context.WithCancel(context.Background())

			ch, err := log.Stream(ctx, "r1")
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			cancel()

			select {
			case _, ok := <-ch:
				if ok {
					t.Fatalf("channel should be closed after cancel")
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("channel not closed within timeout")
			}
		})
	}
}

// -----------------------------------------------------------------------
// Close
// -----------------------------------------------------------------------

func TestContract_Close_BlocksFurtherAppend(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			if err := log.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			var cb chainBuilder
			err := log.Append(context.Background(), "r1", cb.next(t, "r1", "x"))
			if !errors.Is(err, eventlog.ErrLogClosed) {
				t.Fatalf("want ErrLogClosed, got %v", err)
			}
		})
	}
}

func TestContract_Close_Idempotent(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			if err := log.Close(); err != nil {
				t.Fatalf("first Close: %v", err)
			}
			if err := log.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}
		})
	}
}

// -----------------------------------------------------------------------
// ListRuns — matrix lives here; exercised by TestContract_ListRuns_*.
// -----------------------------------------------------------------------

func TestContract_ListRuns_Empty(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			runs, err := log.(eventlog.RunLister).ListRuns(context.Background())
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(runs) != 0 {
				t.Fatalf("got %d runs, want 0", len(runs))
			}
		})
	}
}

func TestContract_ListRuns_PerRunSummary(t *testing.T) {
	completedPayload, err := event.EncodePayload(event.RunCompleted{
		FinalText: "ok", TurnCount: 1,
	})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}

	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			ctx := context.Background()

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

			runs, err := log.(eventlog.RunLister).ListRuns(ctx)
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(runs) != 2 {
				t.Fatalf("got %d runs, want 2: %+v", len(runs), runs)
			}

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
			if r2.TerminalKind != event.KindRunStarted {
				t.Errorf("r2.TerminalKind = %s, want RunStarted", r2.TerminalKind)
			}
			if r2.TerminalKind.IsTerminal() {
				t.Errorf("r2.TerminalKind.IsTerminal() = true, want false")
			}

			wantR1Start := time.Unix(0, 1_000_000)
			if !r1.StartedAt.Equal(wantR1Start) {
				t.Errorf("r1.StartedAt = %v, want %v", r1.StartedAt, wantR1Start)
			}
		})
	}
}

func TestContract_ListRuns_NewestFirst(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			ctx := context.Background()

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
				encoded, err := event.Marshal(ev)
				if err != nil {
					t.Fatalf("Marshal: %v", err)
				}
				cb.prevHash = event.Hash(encoded)
				if err := log.Append(ctx, run.id, ev); err != nil {
					t.Fatalf("append %s: %v", run.id, err)
				}
			}

			runs, err := log.(eventlog.RunLister).ListRuns(ctx)
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

func TestContract_ListRuns_AfterClose(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			log := bk.open(t)
			log.Close()
			if _, err := log.(eventlog.RunLister).ListRuns(context.Background()); err == nil {
				t.Fatal("ListRuns after Close: want error, got nil")
			}
		})
	}
}
