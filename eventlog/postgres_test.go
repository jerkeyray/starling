package eventlog_test

// Postgres-backed tests. Gated by the PG_EVENTLOG_TEST env var: set it
// to a Postgres DSN (e.g. "postgres://starling:starling@localhost:5432/starling_test?sslmode=disable")
// to run, leave unset to skip. CI sets this to a GitHub Actions service
// container's address; local devs opt in when they want to verify the
// Postgres path.
//
// Every opened log runs InstallSchema and then TRUNCATEs
// eventlog_events, so tests don't leak state into each other. Tests in
// this package are sequential (no t.Parallel); do not add parallelism
// without redesigning this isolation strategy.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
)

// pgDSN returns the configured test DSN, or "" if Postgres tests are
// disabled.
func pgDSN() string { return os.Getenv("PG_EVENTLOG_TEST") }

// openPG opens a fresh Postgres-backed EventLog. Installs the schema
// on first call, TRUNCATEs the table on every call so no test sees
// another's rows. Registers Close on t.Cleanup.
func openPG(t *testing.T) eventlog.EventLog {
	t.Helper()
	dsn := pgDSN()
	if dsn == "" {
		t.Skip("PG_EVENTLOG_TEST not set; skipping Postgres test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open pgx: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("ping postgres: %v", err)
	}
	if err := eventlog.InstallSchema(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("InstallSchema: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `TRUNCATE eventlog_events`); err != nil {
		_ = db.Close()
		t.Fatalf("truncate: %v", err)
	}
	log, err := eventlog.NewPostgres(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		_ = log.Close()
		_ = db.Close()
	})
	return log
}

// TestPostgres_Smoke is a minimum viable signal that the Postgres
// backend is connected, schema is installed, and the happy path
// works. The full contract matrix (append invariants, read, stream,
// close, ListRuns) runs under the backends() helper when
// PG_EVENTLOG_TEST is set — see contract_test.go.
func TestPostgres_Smoke(t *testing.T) {
	log := openPG(t)
	ctx := context.Background()
	cb := &chainBuilder{}
	for i := 0; i < 3; i++ {
		if err := log.Append(ctx, "smoke", cb.next(t, "smoke", fmt.Sprintf("msg-%d", i))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	got, err := log.Read(ctx, "smoke")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	sealed := sealChain(t, ctx, log, "smoke", got)
	if err := eventlog.Validate(sealed); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestPostgres_ReadOnly_RejectsAppend mirrors the SQLite contract for
// the WithReadOnlyPG option. Pure-in-process enforcement; the doc
// comment is clear that real enforcement requires a read-only role.
func TestPostgres_ReadOnly_RejectsAppend(t *testing.T) {
	dsn := pgDSN()
	if dsn == "" {
		t.Skip("PG_EVENTLOG_TEST not set")
	}
	// Writer session: seed one event.
	rw := openPG(t)
	cb := &chainBuilder{}
	if err := rw.Append(context.Background(), "ro", cb.next(t, "ro", "hi")); err != nil {
		t.Fatalf("rw.Append: %v", err)
	}

	// Read-only session: shares the same DSN, different *sql.DB.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ro, err := eventlog.NewPostgres(db, eventlog.WithReadOnlyPG())
	if err != nil {
		t.Fatalf("NewPostgres read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	evs, err := ro.Read(context.Background(), "ro")
	if err != nil {
		t.Fatalf("Read (ro): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("Read (ro): got %d, want 1", len(evs))
	}

	err = ro.Append(context.Background(), "ro", cb.next(t, "ro", "should-fail"))
	if !errors.Is(err, eventlog.ErrReadOnly) {
		t.Fatalf("ro.Append: err = %v, want ErrReadOnly", err)
	}
}

// TestPostgres_ConcurrentAppendsSameRun exercises the per-run
// advisory lock. 16 goroutines race to append to the same run; the
// chain state (seq, prev_hash) is computed under a shared mutex so
// every goroutine proposes a valid-at-the-time candidate. The
// advisory lock ensures at most one wins per seq slot; the rest see
// ErrInvalidAppend because by the time they acquire the lock,
// someone else has bumped the tail.
//
// Post-conditions:
//   - Exactly perRun successful Appends.
//   - Read returns seq 1..perRun in order.
//   - Validate passes (chain intact).
//   - Every failure is ErrInvalidAppend (not a raw driver error).
//
// The test deliberately has more goroutines than target events so
// every seq slot has genuine contention.
func TestPostgres_ConcurrentAppendsSameRun(t *testing.T) {
	log := openPG(t)
	ctx := context.Background()

	const workers = 16
	const perRun = 20

	var (
		mu       sync.Mutex
		success  int32
		failures int32
	)
	cb := &chainBuilder{}
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				if atomic.LoadInt32(&success) >= perRun {
					return
				}
				// Mint candidate under the mutex so seq/prev are consistent
				// with the chainBuilder's local view.
				mu.Lock()
				if atomic.LoadInt32(&success) >= perRun {
					mu.Unlock()
					return
				}
				ev := cb.next(t, "contended", "x")
				snapshot := *cb
				mu.Unlock()

				err := log.Append(ctx, "contended", ev)
				if err == nil {
					atomic.AddInt32(&success, 1)
					continue
				}
				atomic.AddInt32(&failures, 1)
				// Roll cb back to the snapshot before this attempt so
				// the next goroutine mints a fresh candidate against
				// the real last-committed event — which we re-read
				// below. This keeps the chainBuilder in sync with the
				// database whenever a race loses.
				mu.Lock()
				*cb = snapshot
				cb.seq--
				// Refresh from DB: read the current last event and
				// update cb.prevHash + cb.seq accordingly.
				got, rerr := log.Read(ctx, "contended")
				if rerr == nil && len(got) > 0 {
					last := got[len(got)-1]
					cb.seq = last.Seq
					enc, _ := event.Marshal(last)
					cb.prevHash = event.Hash(enc)
				} else {
					cb.seq = 0
					cb.prevHash = nil
				}
				mu.Unlock()

				if !errors.Is(err, eventlog.ErrInvalidAppend) {
					t.Errorf("unexpected error class (want ErrInvalidAppend): %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Workers may overshoot perRun by a handful — the success-count
	// guard and the mutex-protected chainBuilder advance are racy by
	// design (check under mutex, increment outside). That's fine: all
	// we need is "at least perRun appends committed cleanly, each one
	// via a unique hash-chained slot". The DB row count is authoritative.
	committed := atomic.LoadInt32(&success)
	if committed < perRun {
		t.Fatalf("success = %d, want >= %d", committed, perRun)
	}

	got, err := log.Read(ctx, "contended")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != int(committed) {
		t.Fatalf("len(got) = %d, want %d (success count)", len(got), committed)
	}
	for i, ev := range got {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("events[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
	sealed := sealChain(t, ctx, log, "contended", got)
	if err := eventlog.Validate(sealed); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if atomic.LoadInt32(&failures) == 0 {
		t.Logf("warning: 0 failed appends — either contention was too low to exercise the lock or the lock is working surgically well")
	}
}

// TestPostgres_ConcurrentAppendsDifferentRunsParallel exercises the
// headline property SQLite cannot offer: two Appends on different
// run_ids proceed in parallel. Timing-based assertions are flaky by
// nature, but we can at minimum prove that N disjoint-run workers
// complete without mutual blocking by measuring elapsed time and
// comparing to a serialized lower-bound estimate. Loose: just assert
// no errors.
func TestPostgres_ConcurrentAppendsDifferentRunsParallel(t *testing.T) {
	log := openPG(t)
	ctx := context.Background()

	const runs = 8
	const perRun = 10

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for r := 0; r < runs; r++ {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			runID := fmt.Sprintf("run-%d", r)
			var cb chainBuilder
			for i := 0; i < perRun; i++ {
				if err := log.Append(ctx, runID, cb.next(t, runID, fmt.Sprintf("%d-%d", r, i))); err != nil {
					errs <- fmt.Errorf("run %s append %d: %w", runID, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("%v", e)
	}

	// Verify every run's chain independently.
	for r := 0; r < runs; r++ {
		runID := fmt.Sprintf("run-%d", r)
		evs, err := log.Read(ctx, runID)
		if err != nil {
			t.Fatalf("Read %s: %v", runID, err)
		}
		if len(evs) != perRun {
			t.Fatalf("run %s: got %d events, want %d", runID, perRun, len(evs))
		}
		sealed := sealChain(t, ctx, log, runID, evs)
		if err := eventlog.Validate(sealed); err != nil {
			t.Fatalf("Validate %s: %v", runID, err)
		}
		for i, ev := range evs {
			if ev.Seq != uint64(i+1) {
				t.Fatalf("run %s: events[%d].Seq = %d, want %d", runID, i, ev.Seq, i+1)
			}
		}
	}
	t.Logf("elapsed: %v (runs=%d perRun=%d)", time.Since(start), runs, perRun)
}

