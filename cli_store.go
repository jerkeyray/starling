package starling

import (
	"fmt"

	"github.com/jerkeyray/starling/eventlog"
)

// openStore opens a SQLite event log at path in read-only mode. It is
// the shared entrypoint used by ValidateCommand, ExportCommand, and
// ReplayCommand so every CLI subcommand treats the log as immutable
// by construction. Returns a wrapped error on open failure.
//
// SQLite is the only backend supported by the built-in CLI commands
// today; Postgres support will land alongside the §1.1 Postgres-ops
// follow-up.
func openStore(path string) (eventlog.EventLog, error) {
	store, err := eventlog.NewSQLite(path, eventlog.WithReadOnly())
	if err != nil {
		return nil, fmt.Errorf("open log %q: %w", path, err)
	}
	return store, nil
}
