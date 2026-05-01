package eventlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// ErrForkNotFound is returned by Fork* helpers when no events match
// the requested run / seq combination in the source log.
var ErrForkNotFound = errors.New("eventlog: fork source not found")

// ForkSQLite copies a SQLite event log at srcPath to dstPath and then
// truncates runID's events to those with seq < beforeSeq, producing a
// fresh "branch point" log that downstream code can append to without
// disturbing the source.
//
// The copy uses SQLite's `VACUUM INTO`, which is the only way to copy
// a live WAL-mode database safely: a plain `cp` of the .db file
// without the .db-wal and .db-shm sidecars produces a corrupt clone.
//
// beforeSeq=0 keeps every event for runID (forks the run as-is). To
// truncate after the first event, pass beforeSeq=2; to keep nothing,
// pass beforeSeq=1.
//
// dstPath must not exist (VACUUM INTO refuses to overwrite). Other
// runs in the source log are copied verbatim; only runID is truncated.
//
// On success the destination is closed; the caller may re-open it via
// NewSQLite. On error any partially-written destination file is
// removed.
func ForkSQLite(ctx context.Context, srcPath, dstPath, runID string, beforeSeq uint64) (err error) {
	if srcPath == "" {
		return errors.New("eventlog/fork: srcPath empty")
	}
	if dstPath == "" {
		return errors.New("eventlog/fork: dstPath empty")
	}
	if srcPath == dstPath {
		return errors.New("eventlog/fork: srcPath == dstPath")
	}
	if runID == "" {
		return errors.New("eventlog/fork: runID empty")
	}
	if _, statErr := os.Stat(dstPath); statErr == nil {
		return fmt.Errorf("eventlog/fork: dstPath %q already exists", dstPath)
	}

	src, err := sql.Open("sqlite", "file:"+srcPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("eventlog/fork: open src: %w", err)
	}
	defer src.Close()

	if err := assertRunPresent(ctx, src, runID); err != nil {
		return err
	}

	if _, err := src.ExecContext(ctx, "VACUUM INTO ?", dstPath); err != nil {
		return fmt.Errorf("eventlog/fork: VACUUM INTO: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dstPath)
			_ = os.Remove(dstPath + "-wal")
			_ = os.Remove(dstPath + "-shm")
		}
	}()

	if beforeSeq == 0 {
		return nil
	}

	dst, err := sql.Open("sqlite", "file:"+dstPath+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("eventlog/fork: open dst: %w", err)
	}
	defer dst.Close()

	if _, err := dst.ExecContext(ctx,
		`DELETE FROM eventlog_events WHERE run_id = ? AND seq >= ?`,
		runID, int64(beforeSeq),
	); err != nil {
		return fmt.Errorf("eventlog/fork: truncate: %w", err)
	}
	return nil
}

func assertRunPresent(ctx context.Context, db *sql.DB, runID string) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM eventlog_events WHERE run_id = ?`, runID,
	).Scan(&n); err != nil {
		return fmt.Errorf("eventlog/fork: probe src: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: run %q has no events", ErrForkNotFound, runID)
	}
	return nil
}
