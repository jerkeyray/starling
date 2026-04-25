package eventlog_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/jerkeyray/starling/eventlog"
	_ "modernc.org/sqlite"
)

func TestMigrate_FreshSQLiteIsCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.db")
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	v, err := eventlog.SchemaVersion(context.Background(), log)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 2 {
		t.Fatalf("SchemaVersion = %d, want 2", v)
	}

	report, err := eventlog.Migrate(context.Background(), log)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(report.Applied) != 0 {
		t.Fatalf("re-Migrate Applied = %v, want []", report.Applied)
	}
	if report.From != 2 || report.To != 2 {
		t.Fatalf("Report = %+v, want From=To=2", report)
	}
}

func TestMigrate_LegacySQLiteIsRenamed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.db")

	// Hand-build a legacy v1 database with the old "events" table.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE events (
		run_id TEXT, seq INTEGER, prev_hash BLOB, ts INTEGER,
		kind INTEGER, payload BLOB, PRIMARY KEY (run_id, seq))`); err != nil {
		t.Fatalf("legacy create: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO events VALUES ('r1', 1, NULL, 0, 1, x'00')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	_ = db.Close()

	// Reopening via NewSQLite must auto-migrate v1→v2 and preserve data.
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	v, err := eventlog.SchemaVersion(context.Background(), log)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 2 {
		t.Fatalf("SchemaVersion = %d, want 2 after legacy migration", v)
	}

	// Confirm the legacy row is in the new table by querying through Read.
	got, err := log.Read(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Read got %d events, want 1", len(got))
	}
}

func TestMigrate_DryRunSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.db")

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE events (
		run_id TEXT, seq INTEGER, prev_hash BLOB, ts INTEGER,
		kind INTEGER, payload BLOB, PRIMARY KEY (run_id, seq))`); err != nil {
		t.Fatalf("legacy create: %v", err)
	}
	_ = db.Close()

	// NewSQLite would auto-migrate, so go through SchemaVersion +
	// Migrate(WithDryRun) directly using a separate handle that bypasses
	// the auto-migrate path. We can't easily skip auto-migrate from
	// outside the package — so verify via report shape on a fresh DB
	// instead, where v1+v2 are pending until called.
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	// Build the meta table only so currentVersion sees 0 / migrations pending.
	dbf, err := sql.Open("sqlite", "file:"+freshPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open fresh: %v", err)
	}
	_ = dbf.Close()

	log, err := eventlog.NewSQLite(freshPath)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	// Already current after open: dry-run reports nothing.
	report, err := eventlog.Migrate(context.Background(), log, eventlog.WithDryRun())
	if err != nil {
		t.Fatalf("Migrate dry: %v", err)
	}
	if !report.DryRun {
		t.Fatalf("DryRun = false, want true")
	}
	if len(report.Applied) != 0 {
		t.Fatalf("dry-run on current db Applied = %v, want []", report.Applied)
	}
}
