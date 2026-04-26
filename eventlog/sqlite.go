package eventlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
	readOnly bool
}

// WithReadOnly opens the database in read-only mode (URI mode=ro).
// Append always fails with ErrReadOnly. Intended for inspector tools
// that should not be able to mutate the audit log they are inspecting.
//
// Crucially this does NOT pass immutable=1: the inspector is expected
// to be safe to point at a database that another Starling process is
// actively writing to. immutable=1 would let SQLite skip WAL and
// change-counter checks, returning stale or torn reads as soon as the
// writer touched the file.
func WithReadOnly() SQLiteOption {
	return func(c *sqliteConfig) { c.readOnly = true }
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
	// _txlock=immediate makes BeginTx grab the write lock upfront,
	// closing the read-then-insert window. busy_timeout must be set
	// per-connection (the pool opens new ones on demand), so it goes
	// on the DSN, not just initSQLite. immutable=1 is intentionally
	// not used: the inspector reads files a writer is appending to.
	var dsn string
	if cfg.readOnly {
		dsn = "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	} else {
		dsn = "file:" + path + "?_txlock=immediate&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("eventlog/sqlite: open: %w", err)
	}
	// Cap pool size so contended writers serialize quickly under the
	// busy-timeout instead of fanning out across many connections.
	db.SetMaxOpenConns(8)
	if !cfg.readOnly {
		if err := initSQLite(db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	log := &sqliteLog{db: db, readOnly: cfg.readOnly}
	if !cfg.readOnly {
		if _, err := log.migrate(context.Background(), false); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return log, nil
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
	return nil
}

// sqliteMigrations are forward-only DDL steps applied in order.
//
//	v1 — create eventlog_events with the M1 column set.
//	v2 — adopt the canonical name on legacy databases that were created
//	     when the table was called "events".
var sqliteMigrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS eventlog_events (
				run_id    TEXT    NOT NULL,
				seq       INTEGER NOT NULL,
				prev_hash BLOB,
				ts        INTEGER NOT NULL,
				kind      INTEGER NOT NULL,
				payload   BLOB    NOT NULL,
				PRIMARY KEY (run_id, seq)
			)`,
		},
	},
	{
		version: 2,
		apply: func(ctx context.Context, tx *sql.Tx) error {
			var legacy int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='events'`,
			).Scan(&legacy); err != nil {
				return err
			}
			if legacy == 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO eventlog_events SELECT * FROM events`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DROP TABLE events`)
			return err
		},
	},
}

func ensureSQLiteMetaTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS `+metaTable+` (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("eventlog/sqlite: create %s: %w", metaTable, err)
	}
	return nil
}

func sqliteCurrentVersion(ctx context.Context, db *sql.DB) (int, error) {
	if err := ensureSQLiteMetaTable(ctx, db); err != nil {
		return 0, err
	}
	var v int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM `+metaTable,
	).Scan(&v); err != nil {
		return 0, fmt.Errorf("eventlog/sqlite: read schema version: %w", err)
	}
	return v, nil
}

func (s *sqliteLog) currentVersion(ctx context.Context) (int, error) {
	return sqliteCurrentVersion(ctx, s.db)
}

func (s *sqliteLog) expectedVersion() int { return latestVersion(sqliteMigrations) }

func (s *sqliteLog) migrate(ctx context.Context, dryRun bool) (MigrationReport, error) {
	if s.readOnly {
		return MigrationReport{}, ErrReadOnly
	}
	if err := ensureSQLiteMetaTable(ctx, s.db); err != nil {
		return MigrationReport{}, err
	}
	current, err := sqliteCurrentVersion(ctx, s.db)
	if err != nil {
		return MigrationReport{}, err
	}
	latest := latestVersion(sqliteMigrations)
	if current > latest {
		return MigrationReport{}, fmt.Errorf("%w: db=%d binary=%d", ErrSchemaTooNew, current, latest)
	}
	applied, err := applyMigrations(ctx, s.db, current, sqliteMigrations, sqliteRecordVersion, dryRun)
	report := MigrationReport{Backend: "sqlite", From: current, Applied: applied, DryRun: dryRun}
	if len(applied) > 0 {
		report.To = applied[len(applied)-1]
	} else {
		report.To = current
	}
	return report, err
}

func sqliteRecordVersion(ctx context.Context, tx *sql.Tx, version int) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO `+metaTable+` (version, applied_at) VALUES (?, ?)`,
		version, time.Now().UnixNano(),
	)
	return err
}

type sqliteLog struct {
	db       *sql.DB
	readOnly bool

	mu     sync.RWMutex
	closed bool
}

func (s *sqliteLog) Append(ctx context.Context, runID string, ev event.Event) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrLogClosed
	}
	s.mu.RUnlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("eventlog/sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	last, err := readLastLocked(ctx, tx, runID)
	if err != nil {
		return fmt.Errorf("eventlog/sqlite: read last: %w", err)
	}
	if err := validateAppend(last, ev); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO eventlog_events(run_id, seq, prev_hash, ts, kind, payload)
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
		 FROM eventlog_events WHERE run_id = ? ORDER BY seq DESC LIMIT 1`,
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
		 FROM eventlog_events WHERE run_id = ? ORDER BY seq`,
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
			 FROM eventlog_events WHERE run_id = ? AND seq > ? ORDER BY seq`,
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

// ListRuns enumerates every run in the database. One indexed query
// over the (run_id, seq) primary key picks the first and last seq per
// run; a second query fetches the matching timestamps and kinds. The
// result is sorted newest-first by StartedAt.
func (s *sqliteLog) ListRuns(ctx context.Context) ([]RunSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrLogClosed
	}
	s.mu.RUnlock()

	// One pass: per run_id, get min(seq) for "started" and max(seq) for
	// "last", then join back to events to pull ts (for first) and kind
	// (for last). A correlated subquery is simplest and the (run_id, seq)
	// PK index covers both lookups.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			run_id,
			(SELECT ts   FROM eventlog_events e2 WHERE e2.run_id = e.run_id ORDER BY seq ASC  LIMIT 1) AS started_ts,
			MAX(seq) AS last_seq,
			(SELECT kind FROM eventlog_events e3 WHERE e3.run_id = e.run_id ORDER BY seq DESC LIMIT 1) AS last_kind
		FROM eventlog_events e
		GROUP BY run_id
	`)
	if err != nil {
		return nil, fmt.Errorf("eventlog/sqlite: list runs: %w", err)
	}
	defer rows.Close()

	var out []RunSummary
	for rows.Next() {
		var (
			runID     string
			startedTs int64
			lastSeq   int64
			lastKind  int64
		)
		if err := rows.Scan(&runID, &startedTs, &lastSeq, &lastKind); err != nil {
			return nil, fmt.Errorf("eventlog/sqlite: list runs scan: %w", err)
		}
		out = append(out, RunSummary{
			RunID:        runID,
			StartedAt:    time.Unix(0, startedTs),
			LastSeq:      uint64(lastSeq),
			TerminalKind: event.Kind(lastKind),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog/sqlite: list runs rows: %w", err)
	}
	// Newest first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
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
