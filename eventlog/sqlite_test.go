package eventlog_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	_ "modernc.org/sqlite"
)

// openSQLite opens a fresh SQLite log under t.TempDir() and registers
// Close on cleanup. Returns the log and the file path (for tamper
// tests that need to reach past the interface).
func openSQLite(t *testing.T) (eventlog.EventLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log.db")
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, path
}

func TestSQLite_AppendReadRoundTrip(t *testing.T) {
	log, _ := openSQLite(t)
	cb := &chainBuilder{}
	want := []event.Event{
		cb.next(t, "run-1", "hello"),
		cb.next(t, "run-1", "world"),
		cb.next(t, "run-1", "!"),
	}
	for _, ev := range want {
		if err := log.Append(context.Background(), "run-1", ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := log.Read(context.Background(), "run-1")
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
		if string(got[i].PrevHash) != string(want[i].PrevHash) {
			t.Errorf("events[%d].PrevHash mismatch", i)
		}
		if string(got[i].Payload) != string(want[i].Payload) {
			t.Errorf("events[%d].Payload mismatch", i)
		}
	}
}

func TestSQLite_ReadUnknownRunReturnsNil(t *testing.T) {
	log, _ := openSQLite(t)
	got, err := log.Read(context.Background(), "missing")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != nil {
		t.Fatalf("got = %v, want nil", got)
	}
}

func TestSQLite_AppendRejectsBadSeq(t *testing.T) {
	log, _ := openSQLite(t)
	cb := &chainBuilder{}
	_ = log.Append(context.Background(), "run-1", cb.next(t, "run-1", "a"))

	// Skip seq=2 — inject an event with Seq=3.
	cb.seq = 2 // simulate drift
	bad := cb.next(t, "run-1", "c")
	err := log.Append(context.Background(), "run-1", bad)
	if !errors.Is(err, eventlog.ErrInvalidAppend) {
		t.Fatalf("err = %v, want ErrInvalidAppend", err)
	}
}

func TestSQLite_AppendRejectsBadPrevHash(t *testing.T) {
	log, _ := openSQLite(t)
	cb := &chainBuilder{}
	_ = log.Append(context.Background(), "run-1", cb.next(t, "run-1", "a"))

	// Tamper the chain builder's prev hash before minting the next event.
	cb.prevHash = []byte("not-a-real-hash-of-previous-event")
	bad := cb.next(t, "run-1", "b")
	err := log.Append(context.Background(), "run-1", bad)
	if !errors.Is(err, eventlog.ErrInvalidAppend) {
		t.Fatalf("err = %v, want ErrInvalidAppend", err)
	}
}

func TestSQLite_AppendAfterCloseReturnsErrLogClosed(t *testing.T) {
	log, _ := openSQLite(t)
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cb := &chainBuilder{}
	err := log.Append(context.Background(), "run-1", cb.next(t, "run-1", "a"))
	if !errors.Is(err, eventlog.ErrLogClosed) {
		t.Fatalf("err = %v, want ErrLogClosed", err)
	}
}

func TestSQLite_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.db")

	// First session: write three events.
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	cb := &chainBuilder{}
	for _, msg := range []string{"a", "b", "c"} {
		if err := log.Append(context.Background(), "run-1", cb.next(t, "run-1", msg)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second session: re-open, read back.
	log2, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer log2.Close()
	got, err := log2.Read(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Read after reopen: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	for i := uint64(1); i <= 3; i++ {
		if got[i-1].Seq != i {
			t.Errorf("events[%d].Seq = %d, want %d", i-1, got[i-1].Seq, i)
		}
	}
}

func TestSQLite_TamperedPayloadFailsValidate(t *testing.T) {
	log, path := openSQLite(t)
	cb := &chainBuilder{}
	for _, msg := range []string{"a", "b", "c"} {
		if err := log.Append(context.Background(), "run-1", cb.next(t, "run-1", msg)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// Force a WAL checkpoint before reaching past the abstraction — a
	// raw UPDATE on the main file only sees committed data.
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Tamper: rewrite the payload for seq=2.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`UPDATE events SET payload = ? WHERE run_id = ? AND seq = ?`,
		[]byte{0x00}, "run-1", 2); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_ = db.Close()

	// Re-open and Validate.
	log2, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer log2.Close()
	got, err := log2.Read(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := eventlog.Validate(got); !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("Validate = %v, want ErrLogCorrupt", err)
	}
}

func TestSQLite_StreamDeliversHistoryThenLive(t *testing.T) {
	log, _ := openSQLite(t)
	cb := &chainBuilder{}
	// Pre-append 2 events before subscribing.
	_ = log.Append(context.Background(), "run-1", cb.next(t, "run-1", "a"))
	_ = log.Append(context.Background(), "run-1", cb.next(t, "run-1", "b"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := log.Stream(ctx, "run-1")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Drain the 2 historical events.
	for i := uint64(1); i <= 2; i++ {
		select {
		case ev := <-ch:
			if ev.Seq != i {
				t.Fatalf("history[%d].Seq = %d, want %d", i, ev.Seq, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("history %d: timeout", i)
		}
	}

	// Append a live event; it should land within a few poll intervals.
	if err := log.Append(context.Background(), "run-1", cb.next(t, "run-1", "c")); err != nil {
		t.Fatalf("Append live: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Seq != 3 {
			t.Fatalf("live Seq = %d, want 3", ev.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live event not delivered within 2s")
	}
}

// TestSQLite_ReadOnly_SeesLiveWrites pins the "agent-while-inspector"
// contract: a read-only handle opened via WithReadOnly must observe
// rows that another handle inserts after it opened. WAL mode makes
// this work; we'd lose it if WithReadOnly ever started passing
// immutable=1 (which tells SQLite the file cannot change and lets it
// skip the change-counter check).
func TestSQLite_ReadOnly_SeesLiveWrites(t *testing.T) {
	rw, path := openSQLite(t)
	cb := &chainBuilder{}
	if err := rw.Append(context.Background(), "r1", cb.next(t, "r1", "first")); err != nil {
		t.Fatalf("Append (rw, first): %v", err)
	}

	ro, err := eventlog.NewSQLite(path, eventlog.WithReadOnly())
	if err != nil {
		t.Fatalf("NewSQLite read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	// First read sees the one event we already wrote.
	first, err := ro.Read(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Read (ro, before live): %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("Read (ro, before live): got %d events, want 1", len(first))
	}

	// Writer keeps appending after the read-only handle was opened.
	if err := rw.Append(context.Background(), "r1", cb.next(t, "r1", "second")); err != nil {
		t.Fatalf("Append (rw, second): %v", err)
	}

	// Second read on the SAME read-only handle must observe the new
	// row — proves we are not holding a stale snapshot or skipping
	// change-counter validation.
	second, err := ro.Read(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Read (ro, after live): %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("Read (ro, after live): got %d events, want 2 — read-only handle did not pick up live write", len(second))
	}
}

// TestSQLite_ReadOnly_RejectsAppend pins the contract that an
// inspector-style consumer opening with WithReadOnly cannot mutate
// the log even by accident.
func TestSQLite_ReadOnly_RejectsAppend(t *testing.T) {
	// First create a writable log with one run, then close and
	// re-open read-only.
	rw, path := openSQLite(t)
	cb := &chainBuilder{}
	if err := rw.Append(context.Background(), "r1", cb.next(t, "r1", "hi")); err != nil {
		t.Fatalf("Append (rw): %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("rw.Close: %v", err)
	}

	ro, err := eventlog.NewSQLite(path, eventlog.WithReadOnly())
	if err != nil {
		t.Fatalf("NewSQLite read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	// Read still works.
	evs, err := ro.Read(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Read (ro): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("Read (ro): got %d events, want 1", len(evs))
	}

	// Append must be rejected with ErrReadOnly.
	err = ro.Append(context.Background(), "r1", cb.next(t, "r1", "should-fail"))
	if !errors.Is(err, eventlog.ErrReadOnly) {
		t.Fatalf("Append (ro): err = %v, want ErrReadOnly", err)
	}
}
