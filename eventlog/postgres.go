package eventlog

// Postgres-backed EventLog. Parity with NewSQLite plus multi-process,
// multi-host concurrent writers. Caller brings a *sql.DB; we don't
// import a driver, so SQLite-only users don't pay the pgx cost.
//
// Each Append runs inside a transaction that first takes
// pg_advisory_xact_lock(hashtextextended(run_id, 0)), serializing
// writers on the same run_id. Stream polls at streamPollInterval;
// LISTEN/NOTIFY is deferred. WithReadOnlyPG is in-process defence in
// depth — for real enforcement, revoke INSERT/UPDATE/DELETE at the
// database role.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
)

// pgStreamPollInterval mirrors sqliteStreamPollInterval — same
// rationale, different constant so we can tune independently.
const pgStreamPollInterval = 50 * time.Millisecond

// pgMigrations are forward-only DDL steps applied by Migrate. v1
// creates eventlog_events; tables managed externally before migrations
// existed are detected and stamped as v1.
var pgMigrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS eventlog_events (
                run_id     TEXT     NOT NULL,
                seq        BIGINT   NOT NULL,
                prev_hash  BYTEA,
                ts         BIGINT   NOT NULL,
                kind       INTEGER  NOT NULL,
                payload    BYTEA    NOT NULL,
                PRIMARY KEY (run_id, seq)
            )`,
		},
	},
}

// PostgresOption configures a Postgres-backed EventLog.
type PostgresOption func(*postgresConfig)

type postgresConfig struct {
	readOnly    bool
	autoMigrate bool
}

// WithReadOnlyPG opens the log in read-only mode: Append always fails
// with ErrReadOnly. Intended for inspector tools that should not be
// able to mutate the audit log they are inspecting.
//
// Defence-in-depth only: the actual enforcement comes from the
// database role you connect as. Grant only SELECT on eventlog_events
// for true read-only.
func WithReadOnlyPG() PostgresOption {
	return func(c *postgresConfig) { c.readOnly = true }
}

// WithAutoMigratePG runs InstallSchema at NewPostgres time. Convenient
// for tests and single-binary deployments; skip it when you manage
// schema via Flyway / goose / atlas / etc. Implied off on read-only.
func WithAutoMigratePG() PostgresOption {
	return func(c *postgresConfig) { c.autoMigrate = true }
}

// InstallSchema brings db up to the latest schema version. Idempotent.
// Equivalent to calling Migrate on a *postgresLog handle wrapping db.
func InstallSchema(ctx context.Context, db *sql.DB) error {
	if err := ensurePGMetaTable(ctx, db); err != nil {
		return err
	}
	current, err := pgCurrentVersion(ctx, db)
	if err != nil {
		return err
	}
	latest := latestVersion(pgMigrations)
	if current > latest {
		return fmt.Errorf("%w: db=%d binary=%d", ErrSchemaTooNew, current, latest)
	}
	if _, err := applyMigrations(ctx, db, current, pgMigrations, pgRecordVersion, false); err != nil {
		return fmt.Errorf("eventlog/postgres: install schema: %w", err)
	}
	return nil
}

func ensurePGMetaTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS `+metaTable+` (
            version    INTEGER PRIMARY KEY,
            applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
        )`)
	if err != nil {
		return fmt.Errorf("eventlog/postgres: create %s: %w", metaTable, err)
	}
	return nil
}

func pgCurrentVersion(ctx context.Context, db *sql.DB) (int, error) {
	if err := ensurePGMetaTable(ctx, db); err != nil {
		return 0, err
	}
	var v int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM `+metaTable,
	).Scan(&v); err != nil {
		return 0, fmt.Errorf("eventlog/postgres: read schema version: %w", err)
	}
	if v == 0 {
		// Legacy database created with the pre-migration InstallSchema:
		// the events table exists but no migration row was recorded.
		var legacy int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables
                WHERE table_name = 'eventlog_events'`,
		).Scan(&legacy); err != nil {
			return 0, fmt.Errorf("eventlog/postgres: detect legacy schema: %w", err)
		}
		if legacy > 0 {
			if _, err := db.ExecContext(ctx,
				`INSERT INTO `+metaTable+` (version) VALUES (1) ON CONFLICT DO NOTHING`,
			); err != nil {
				return 0, fmt.Errorf("eventlog/postgres: stamp v1: %w", err)
			}
			return 1, nil
		}
	}
	return v, nil
}

func pgRecordVersion(ctx context.Context, tx *sql.Tx, version int) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO `+metaTable+` (version) VALUES ($1)`,
		version,
	)
	return err
}

func (p *postgresLog) currentVersion(ctx context.Context) (int, error) {
	return pgCurrentVersion(ctx, p.db)
}

func (p *postgresLog) expectedVersion() int { return latestVersion(pgMigrations) }

func (p *postgresLog) migrate(ctx context.Context, dryRun bool) (MigrationReport, error) {
	if p.readOnly {
		return MigrationReport{}, ErrReadOnly
	}
	if err := ensurePGMetaTable(ctx, p.db); err != nil {
		return MigrationReport{}, err
	}
	current, err := pgCurrentVersion(ctx, p.db)
	if err != nil {
		return MigrationReport{}, err
	}
	latest := latestVersion(pgMigrations)
	if current > latest {
		return MigrationReport{}, fmt.Errorf("%w: db=%d binary=%d", ErrSchemaTooNew, current, latest)
	}
	applied, err := applyMigrations(ctx, p.db, current, pgMigrations, pgRecordVersion, dryRun)
	report := MigrationReport{Backend: "postgres", From: current, Applied: applied, DryRun: dryRun}
	if len(applied) > 0 {
		report.To = applied[len(applied)-1]
	} else {
		report.To = current
	}
	return report, err
}

// NewPostgres returns an EventLog backed by Postgres. The caller owns
// the *sql.DB: its Close is NOT called by postgresLog.Close, and its
// connection-pool / TLS / retry config is the caller's concern.
//
// The caller is also responsible for having run InstallSchema before
// the first Append — or passing WithAutoMigratePG to have NewPostgres
// do it.
func NewPostgres(db *sql.DB, opts ...PostgresOption) (EventLog, error) {
	if db == nil {
		return nil, errors.New("eventlog/postgres: nil *sql.DB")
	}
	cfg := postgresConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.autoMigrate && !cfg.readOnly {
		if err := InstallSchema(context.Background(), db); err != nil {
			return nil, err
		}
	}
	return &postgresLog{db: db, readOnly: cfg.readOnly}, nil
}

type postgresLog struct {
	db       *sql.DB
	readOnly bool

	mu     sync.RWMutex
	closed bool
}

func (p *postgresLog) Append(ctx context.Context, runID string, ev event.Event) error {
	if p.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return ErrLogClosed
	}
	p.mu.RUnlock()

	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("eventlog/postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Per-run mutual exclusion. hashtextextended is a stable 64-bit
	// hash over TEXT, available since Postgres 11. The lock is held
	// until this tx commits/rollbacks.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, runID,
	); err != nil {
		return fmt.Errorf("eventlog/postgres: advisory lock: %w", err)
	}

	last, err := pgReadLastLocked(ctx, tx, runID)
	if err != nil {
		return fmt.Errorf("eventlog/postgres: read last: %w", err)
	}
	if err := validateAppend(runID, last, ev); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO eventlog_events(run_id, seq, prev_hash, ts, kind, payload)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		ev.RunID, int64(ev.Seq), ev.PrevHash, ev.Timestamp, int64(ev.Kind), []byte(ev.Payload),
	); err != nil {
		// Belt-and-suspenders: if the advisory lock was ever bypassed
		// two concurrent inserts at the same seq would collide on the
		// PK. Translate to ErrInvalidAppend so callers can unify their
		// retry logic with other validation failures.
		if isPgUniqueViolation(err) {
			return fmt.Errorf("%w: duplicate (run_id, seq) — concurrent append race", ErrInvalidAppend)
		}
		return fmt.Errorf("eventlog/postgres: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("eventlog/postgres: commit: %w", err)
	}
	return nil
}

// pgReadLastLocked returns the last event of runID inside the append
// tx. Because the caller already holds the run's advisory xact lock,
// this read is effectively mutually exclusive with any other writer
// on the same run.
func pgReadLastLocked(ctx context.Context, tx *sql.Tx, runID string) (*event.Event, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT seq, prev_hash, ts, kind, payload
		 FROM eventlog_events WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`,
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

// isPgUniqueViolation reports whether err is a Postgres 23505
// unique_violation. Uses string matching so we don't force a pgx
// *pgconn.PgError assertion — pq and pgx both surface the code in the
// error text via "SQLSTATE 23505" or similar.
func isPgUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "23505") || strings.Contains(s, "unique_violation")
}

func (p *postgresLog) Read(ctx context.Context, runID string) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, ErrLogClosed
	}
	p.mu.RUnlock()

	rows, err := p.db.QueryContext(ctx,
		`SELECT seq, prev_hash, ts, kind, payload
		 FROM eventlog_events WHERE run_id = $1 ORDER BY seq`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("eventlog/postgres: query: %w", err)
	}
	defer rows.Close()

	var out []event.Event
	for rows.Next() {
		ev, err := scanEvent(rows, runID)
		if err != nil {
			return nil, fmt.Errorf("eventlog/postgres: scan: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog/postgres: rows: %w", err)
	}
	return out, nil
}

func (p *postgresLog) Stream(ctx context.Context, runID string) (<-chan event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, ErrLogClosed
	}
	p.mu.RUnlock()

	ch := make(chan event.Event, streamBufferSize)
	go p.streamLoop(ctx, runID, ch)
	return ch, nil
}

// streamLoop polls every pgStreamPollInterval for rows with
// seq > lastSeq and emits them in order. Exits and closes ch on ctx
// cancellation, log closure, or a query error. Subscribers lag up to
// one poll interval behind writers.
//
// Correctness of subscribe-then-deliver: Stream returns the channel
// after starting this goroutine; the first emit() runs before any
// ticker event, and it sees all rows with seq > 0. A subscriber that
// calls Stream after N events and before the N+1 Append gets every
// historical event on its first emit tick. Any Append made strictly
// after Stream returned lands in a later emit tick.
func (p *postgresLog) streamLoop(ctx context.Context, runID string, ch chan<- event.Event) {
	defer close(ch)

	var lastSeq int64
	ticker := time.NewTicker(pgStreamPollInterval)
	defer ticker.Stop()

	emit := func() bool {
		rows, err := p.db.QueryContext(ctx,
			`SELECT seq, prev_hash, ts, kind, payload
			 FROM eventlog_events WHERE run_id = $1 AND seq > $2 ORDER BY seq`,
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

	if !emit() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.RLock()
			closed := p.closed
			p.mu.RUnlock()
			if closed {
				return
			}
			if !emit() {
				return
			}
		}
	}
}

// ListRuns returns a RunSummary per run present in the database.
// Uses the same correlated-subquery shape as SQLite for readability;
// the (run_id, seq) PK covers both subqueries.
func (p *postgresLog) ListRuns(ctx context.Context) ([]RunSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, ErrLogClosed
	}
	p.mu.RUnlock()

	rows, err := p.db.QueryContext(ctx, `
		SELECT
			e.run_id,
			(SELECT ts   FROM eventlog_events e2 WHERE e2.run_id = e.run_id ORDER BY seq ASC  LIMIT 1) AS started_ts,
			MAX(seq) AS last_seq,
			(SELECT kind FROM eventlog_events e3 WHERE e3.run_id = e.run_id ORDER BY seq DESC LIMIT 1) AS last_kind
		FROM eventlog_events e
		GROUP BY e.run_id
	`)
	if err != nil {
		return nil, fmt.Errorf("eventlog/postgres: list runs: %w", err)
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
			return nil, fmt.Errorf("eventlog/postgres: list runs scan: %w", err)
		}
		out = append(out, RunSummary{
			RunID:        runID,
			StartedAt:    time.Unix(0, startedTs),
			LastSeq:      uint64(lastSeq),
			TerminalKind: event.Kind(lastKind),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog/postgres: list runs rows: %w", err)
	}
	// See sqliteLog.ListRuns for the per-run aggregate cost note.
	for i := range out {
		evs, err := p.Read(ctx, out[i].RunID)
		if err != nil {
			return nil, fmt.Errorf("eventlog/postgres: list runs aggregate: %w", err)
		}
		t, tc, in, oTok, cost, durNs := aggregateRun(evs)
		out[i].TurnCount = t
		out[i].ToolCallCount = tc
		out[i].InputTokens = in
		out[i].OutputTokens = oTok
		out[i].CostUSD = cost
		out[i].DurationMs = durNs / 1_000_000
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

// Close marks the log closed. It deliberately does NOT close the
// underlying *sql.DB; the caller passed that pool in and owns its
// lifecycle. Idempotent.
func (p *postgresLog) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return nil
}
