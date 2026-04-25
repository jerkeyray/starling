package eventlog_test

// SQLite-specific tests. Shared Append/Read/Stream/Close/ListRuns
// contract coverage lives in contract_test.go; this file is reserved
// for behaviour that only makes sense against the SQLite backend:
// on-disk persistence, tamper detection on a real file, and the
// read-only-while-live-writer contract that relies on SQLite WAL.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jerkeyray/starling/eventlog"
	_ "modernc.org/sqlite"
)

func TestSQLite_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.db")

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
	path := filepath.Join(t.TempDir(), "log.db")
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
	// Force a WAL checkpoint via Close — a raw UPDATE on the main
	// file only sees committed data.
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`UPDATE events SET payload = ? WHERE run_id = ? AND seq = ?`,
		[]byte{0x00}, "run-1", 2); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_ = db.Close()

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

// TestSQLite_ReadOnly_SeesLiveWrites pins the "agent-while-inspector"
// contract: a read-only handle opened via WithReadOnly must observe
// rows that another handle inserts after it opened. WAL mode makes
// this work; we'd lose it if WithReadOnly ever started passing
// immutable=1 (which tells SQLite the file cannot change and lets it
// skip the change-counter check).
func TestSQLite_ReadOnly_SeesLiveWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.db")
	rw, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = rw.Close() })

	cb := &chainBuilder{}
	if err := rw.Append(context.Background(), "r1", cb.next(t, "r1", "first")); err != nil {
		t.Fatalf("Append (rw, first): %v", err)
	}

	ro, err := eventlog.NewSQLite(path, eventlog.WithReadOnly())
	if err != nil {
		t.Fatalf("NewSQLite read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	first, err := ro.Read(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Read (ro, before live): %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("Read (ro, before live): got %d events, want 1", len(first))
	}

	if err := rw.Append(context.Background(), "r1", cb.next(t, "r1", "second")); err != nil {
		t.Fatalf("Append (rw, second): %v", err)
	}

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
	path := filepath.Join(t.TempDir(), "log.db")
	rw, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
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

	evs, err := ro.Read(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Read (ro): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("Read (ro): got %d events, want 1", len(evs))
	}

	err = ro.Append(context.Background(), "r1", cb.next(t, "r1", "should-fail"))
	if !errors.Is(err, eventlog.ErrReadOnly) {
		t.Fatalf("Append (ro): err = %v, want ErrReadOnly", err)
	}
}

// TestSQLite_ConcurrentAppendSerializes asserts that two appenders
// racing on the same tail cannot both succeed and that the loser fails
// with ErrInvalidAppend, not a raw PRIMARY KEY violation.
func TestSQLite_ConcurrentAppendSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.db")
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	// Seed one event so both contenders observe the same tail.
	seed := &chainBuilder{}
	if err := log.Append(context.Background(), "r1", seed.next(t, "r1", "seed")); err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	// Two independent chainBuilders, each starting from the seed tail,
	// each minting their own seq=2 event. Only one INSERT can win.
	mkContender := func() func() error {
		cb := &chainBuilder{seq: seed.seq, prevHash: seed.prevHash}
		ev := cb.next(t, "r1", "race")
		return func() error { return log.Append(context.Background(), "r1", ev) }
	}
	a, b := mkContender(), mkContender()

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		errs      []error
		successes int
	)
	run := func(fn func() error) {
		defer wg.Done()
		err := fn()
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			successes++
			return
		}
		errs = append(errs, err)
	}
	wg.Add(2)
	go run(a)
	go run(b)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (errs=%v)", successes, errs)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one error", errs)
	}
	if !errors.Is(errs[0], eventlog.ErrInvalidAppend) {
		t.Fatalf("losing append err = %v, want ErrInvalidAppend", errs[0])
	}

	got, err := log.Read(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (seed + one winner)", len(got))
	}
}
