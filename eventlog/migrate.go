package eventlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// metaTable is the per-database table that records which migrations
// have been applied. Both backends share the name so dump/restore
// across backends is straightforward.
const metaTable = "eventlog_schema_migrations"

// ErrSchemaTooNew is returned when the database reports a schema
// version higher than this binary's latest migration.
var ErrSchemaTooNew = errors.New("eventlog: database schema is newer than this binary")

// ErrSchemaOutdated is returned by Preflight when the database is at a
// lower schema version than the running binary expects. Run `starling
// migrate <db>` to remediate.
var ErrSchemaOutdated = errors.New("eventlog: database schema is older than this binary; run migrate")

// MigrationReport describes what Migrate did (or, with WithDryRun,
// would have done).
type MigrationReport struct {
	Backend string
	From    int
	To      int
	Applied []int
	DryRun  bool
}

// MigrateOption tunes Migrate.
type MigrateOption func(*migrateConfig)

type migrateConfig struct {
	dryRun bool
}

// WithDryRun makes Migrate report what it would do without applying
// any DDL. The returned MigrationReport.DryRun is true.
func WithDryRun() MigrateOption {
	return func(c *migrateConfig) { c.dryRun = true }
}

// SchemaVersion returns the highest migration version recorded in the
// database backing log. A fresh database that was never migrated
// returns 0.
func SchemaVersion(ctx context.Context, log EventLog) (int, error) {
	m, ok := log.(migrater)
	if !ok {
		return 0, errNoMigrations(log)
	}
	return m.currentVersion(ctx)
}

// Migrate brings the database backing log up to the latest version
// known to this binary. Forward-only. Idempotent.
func Migrate(ctx context.Context, log EventLog, opts ...MigrateOption) (MigrationReport, error) {
	cfg := migrateConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	m, ok := log.(migrater)
	if !ok {
		return MigrationReport{}, errNoMigrations(log)
	}
	return m.migrate(ctx, cfg.dryRun)
}

// Preflight verifies the database backing log is at the latest schema
// version the running binary expects. Logs that don't track schema
// (e.g. in-memory) return nil. Returns ErrSchemaOutdated or
// ErrSchemaTooNew on mismatch.
func Preflight(ctx context.Context, log EventLog) error {
	m, ok := log.(migrater)
	if !ok {
		return nil
	}
	current, err := m.currentVersion(ctx)
	if err != nil {
		return err
	}
	expected := m.expectedVersion()
	switch {
	case current > expected:
		return fmt.Errorf("%w: db=%d binary=%d", ErrSchemaTooNew, current, expected)
	case current < expected:
		return fmt.Errorf("%w: db=%d binary=%d", ErrSchemaOutdated, current, expected)
	}
	return nil
}

// migrater is the internal interface a concrete backend implements so
// SchemaVersion / Migrate / Preflight can dispatch without widening
// the EventLog interface.
type migrater interface {
	currentVersion(ctx context.Context) (int, error)
	migrate(ctx context.Context, dryRun bool) (MigrationReport, error)
	expectedVersion() int
}

func errNoMigrations(log EventLog) error {
	return fmt.Errorf("eventlog: log %T does not support migrations", log)
}

// migration is one forward-only step. Each backend supplies its own
// SQL because table-creation syntax differs (BLOB vs BYTEA, AUTOINCREMENT, …).
// stmts run in order; apply, when set, runs after for migrations that
// need procedural logic (e.g. conditional rename).
type migration struct {
	version int
	stmts   []string
	apply   func(ctx context.Context, tx *sql.Tx) error
}

// latestVersion returns the highest migration version in steps.
func latestVersion(steps []migration) int {
	highest := 0
	for _, s := range steps {
		if s.version > highest {
			highest = s.version
		}
	}
	return highest
}

// applyMigrations runs each pending migration step inside its own
// transaction, recording success via recordVersion. Stops at the
// first failure.
func applyMigrations(
	ctx context.Context,
	db *sql.DB,
	current int,
	steps []migration,
	recordVersion func(ctx context.Context, tx *sql.Tx, version int) error,
	dryRun bool,
) ([]int, error) {
	var applied []int
	for _, step := range steps {
		if step.version <= current {
			continue
		}
		if dryRun {
			applied = append(applied, step.version)
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return applied, fmt.Errorf("begin v%d: %w", step.version, err)
		}
		for _, stmt := range step.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return applied, fmt.Errorf("apply v%d: %w", step.version, err)
			}
		}
		if step.apply != nil {
			if err := step.apply(ctx, tx); err != nil {
				_ = tx.Rollback()
				return applied, fmt.Errorf("apply v%d: %w", step.version, err)
			}
		}
		if err := recordVersion(ctx, tx, step.version); err != nil {
			_ = tx.Rollback()
			return applied, fmt.Errorf("record v%d: %w", step.version, err)
		}
		if err := tx.Commit(); err != nil {
			return applied, fmt.Errorf("commit v%d: %w", step.version, err)
		}
		applied = append(applied, step.version)
	}
	return applied, nil
}
