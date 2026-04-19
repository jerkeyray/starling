package eventlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
	_ "modernc.org/sqlite"
)

// sqliteStreamPollInterval bounds how long a Stream subscriber lags
// behind the latest append. Shorter intervals cut latency but raise
// idle query load. 50ms balances the two for interactive agent runs.
const sqliteStreamPollInterval = 50 * time.Millisecond

// SQLiteOption configures a SQLite-backed EventLog.
type SQLiteOption func(*sqliteConfig)

type sqliteConfig struct {
	// Reserved for future knobs (e.g. custom poll interval, read-only).
}

// NewSQLite opens (or creates) a SQLite database at path and returns
// an EventLog backed by it. WAL mode and synchronous=NORMAL are
// enabled for concurrent read + single-writer throughput. The schema
// is created on first open and is backwards-compatible across
// re-opens of the same file.
//
// Pass ":memory:" as path for an ephemeral database.
func NewSQLite(path string, opts ...SQLiteOption) (EventLog, error) {
	cfg := sqliteConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("eventlog/sqlite: open: %w", err)
	}
	// A single writer keeps BEGIN IMMEDIATE simple; readers come in on
	// separate connections from the driver pool.
	db.SetMaxOpenConns(8)
	if err := initSQLite(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqliteLog{db: db}, nil
}

func initSQLite(db *sql.DB) error {
	pragmas := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA foreign_keys=ON`,
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("eventlog/sqlite: %s: %w", p, err)
		}
	}
	schema := `CREATE TABLE IF NOT EXISTS events (
		run_id    TEXT    NOT NULL,
		seq       INTEGER NOT NULL,
		prev_hash BLOB,
		ts        INTEGER NOT NULL,
		kind      INTEGER NOT NULL,
		payload   BLOB    NOT NULL,
		PRIMARY KEY (run_id, seq)
	)`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("eventlog/sqlite: create schema: %w", err)
	}
	return nil
}

type sqliteLog struct {
	db *sql.DB

	mu     sync.RWMutex
	closed bool
}

func (s *sqliteLog) Append(ctx context.Context, runID string, ev event.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrLogClosed
	}
	s.mu.RUnlock()

	// BEGIN IMMEDIATE acquires the write lock up front so two
	// concurrent Appends serialize here rather than racing the
	// read-then-insert.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("eventlog/sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		// modernc.org/sqlite's BeginTx already starts a DEFERRED
		// transaction, so an explicit BEGIN IMMEDIATE inside it errors.
		// That's fine — DEFERRED escalates to a write lock on the first
		// INSERT, which still serializes writers. Swallow the error.
		_ = err
	}

	last, err := readLastLocked(ctx, tx, runID)
	if err != nil {
		return fmt.Errorf("eventlog/sqlite: read last: %w", err)
	}
	if err := validateAppend(last, ev); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(run_id, seq, prev_hash, ts, kind, payload)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ev.RunID, int64(ev.Seq), ev.PrevHash, ev.Timestamp, int64(ev.Kind), []byte(ev.Payload),
	); err != nil {
		return fmt.Errorf("eventlog/sqlite: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("eventlog/sqlite: commit: %w", err)
	}
	return nil
}

// readLastLocked returns the last event of runID or nil if the run is
// empty. Called inside an Append transaction; uses the tx handle so
// it sees the same snapshot INSERT will write against.
func readLastLocked(ctx context.Context, tx *sql.Tx, runID string) (*event.Event, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT seq, prev_hash, ts, kind, payload
		 FROM events WHERE run_id = ? ORDER BY seq DESC LIMIT 1`,
		runID,
	)
	ev, err := scanEvent(row, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// rowScanner is the subset of *sql.Row / *sql.Rows we need.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(r rowScanner, runID string) (event.Event, error) {
	var (
		seq     int64
		prev    []byte
		ts      int64
		kind    int64
		payload []byte
	)
	if err := r.Scan(&seq, &prev, &ts, &kind, &payload); err != nil {
		return event.Event{}, err
	}
	return event.Event{
		RunID:     runID,
		Seq:       uint64(seq),
		PrevHash:  prev,
		Timestamp: ts,
		Kind:      event.Kind(kind),
		Payload:   payload,
	}, nil
}

func (s *sqliteLog) Read(ctx context.Context, runID string) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrLogClosed
	}
	s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, prev_hash, ts, kind, payload
		 FROM events WHERE run_id = ? ORDER BY seq`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("eventlog/sqlite: query: %w", err)
	}
	defer rows.Close()

	var out []event.Event
	for rows.Next() {
		ev, err := scanEvent(rows, runID)
		if err != nil {
			return nil, fmt.Errorf("eventlog/sqlite: scan: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog/sqlite: rows: %w", err)
	}
	return out, nil
}

func (s *sqliteLog) Stream(ctx context.Context, runID string) (<-chan event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrLogClosed
	}
	s.mu.RUnlock()

	ch := make(chan event.Event, streamBufferSize)
	go s.streamLoop(ctx, runID, ch)
	return ch, nil
}

// streamLoop polls the events table every sqliteStreamPollInterval
// for rows with seq > lastSeq and emits them in order. Exits and
// closes ch on ctx cancellation, log closure, or a query error.
// Subscribers can lag up to one poll interval behind writers.
func (s *sqliteLog) streamLoop(ctx context.Context, runID string, ch chan<- event.Event) {
	defer close(ch)

	var lastSeq int64
	ticker := time.NewTicker(sqliteStreamPollInterval)
	defer ticker.Stop()

	emit := func() bool {
		rows, err := s.db.QueryContext(ctx,
			`SELECT seq, prev_hash, ts, kind, payload
			 FROM events WHERE run_id = ? AND seq > ? ORDER BY seq`,
			runID, lastSeq,
		)
		if err != nil {
			return false
		}
		defer rows.Close()
		for rows.Next() {
			ev, err := scanEvent(rows, runID)
			if err != nil {
				return false
			}
			select {
			case ch <- ev:
				lastSeq = int64(ev.Seq)
			case <-ctx.Done():
				return false
			}
		}
		return rows.Err() == nil
	}

	// Drain history once before entering the poll loop so subscribers
	// always see every historical event regardless of timing.
	if !emit() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.RLock()
			closed := s.closed
			s.mu.RUnlock()
			if closed {
				return
			}
			if !emit() {
				return
			}
		}
	}
}

func (s *sqliteLog) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}
