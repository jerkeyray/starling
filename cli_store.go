package starling

import (
	"fmt"

	"github.com/jerkeyray/starling/eventlog"
)

// openStore opens a SQLite event log at path read-only. Shared by
// every built-in CLI subcommand so the log is immutable by
// construction.
func openStore(path string) (eventlog.EventLog, error) {
	store, err := eventlog.NewSQLite(path, eventlog.WithReadOnly())
	if err != nil {
		return nil, fmt.Errorf("open log %q: %w", path, err)
	}
	return store, nil
}
