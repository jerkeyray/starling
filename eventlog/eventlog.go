// Package eventlog defines the EventLog interface and ships three
// default backends: in-memory (NewInMemory), SQLite (NewSQLite), and
// Postgres (NewPostgres). All three satisfy the same Append/Read/
// Stream/Close contract; the SQLite and Postgres backends additionally
// implement RunLister and support a read-only mode for inspector-style
// consumers that must not be able to mutate the audit log.
package eventlog

import (
	"context"
	"errors"
	"time"

	"github.com/jerkeyray/starling/event"
)

// RunSummary is a per-run capsule returned by EventLog.ListRuns. It is
// deliberately small: enough to populate a "list of runs" view in a
// debugger or dashboard without loading every event for every run.
//
// TerminalKind is the Kind of the run's last event. For an in-progress
// run the last event will not be terminal; callers should consult
// Kind.IsTerminal to distinguish "still running" from "ended cleanly /
// failed / cancelled".
type RunSummary struct {
	RunID        string     // The run identifier.
	StartedAt    time.Time  // Wall-clock timestamp of the first event.
	LastSeq      uint64     // Sequence number of the most recent event.
	TerminalKind event.Kind // Kind of the most recent event (terminal or not).
}

// EventLog is an append-only, per-run ledger of events.
//
// Implementations must enforce the hash-chain invariants on Append: the first
// event of a run must have Seq == 1 and empty PrevHash; every subsequent
// event must have Seq == prev.Seq+1 and PrevHash == event.Hash(event.Marshal(prev)).
// The caller (typically the step package) is responsible for computing Seq
// and PrevHash before calling Append; the log validates but does not assign.
type EventLog interface {
	// Append validates and stores ev as the next event in runID's chain.
	// Returns an error wrapping ErrInvalidAppend on invariant violation, or
	// ErrLogClosed if the log has been closed.
	Append(ctx context.Context, runID string, ev event.Event) error

	// Read returns every event stored for runID, in sequence order.
	// Unknown runID returns (nil, nil). The returned slice is safe for the
	// caller to mutate.
	Read(ctx context.Context, runID string) ([]event.Event, error)

	// Stream returns a channel that receives every event for runID: first
	// any events already stored (historical replay), then any events
	// appended after subscription (live). The channel is closed when ctx is
	// cancelled, when the log is closed, or when the subscriber falls far
	// enough behind that its buffer overflows.
	//
	// Ordering caveat: strict history-then-live delivery is only
	// guaranteed when no Append is in flight. Under concurrent Appends,
	// backends may interleave late history with live events (the
	// in-memory backend's long-history pump is the canonical example —
	// see memory.go's Stream). Consumers that need exactly-once,
	// monotonically-increasing Seq delivery must track the highest Seq
	// received and discard anything ev.Seq <= lastSeen. inspect/live.go
	// is the reference pattern; its
	// TestLiveStream_LongHistory_ConcurrentAppend exercises this path.
	Stream(ctx context.Context, runID string) (<-chan event.Event, error)

	// Close releases all resources and closes every live subscriber
	// channel. Idempotent.
	Close() error
}

// RunLister is implemented by EventLog backends that can enumerate the
// runs they hold. It is intentionally separate from EventLog so custom
// backends (write-only sinks, network forwarders, ...) are not forced
// to support enumeration. Inspector-style consumers type-assert:
//
//	if lister, ok := log.(eventlog.RunLister); ok {
//	    runs, err := lister.ListRuns(ctx)
//	    ...
//	}
//
// All built-in backends (NewInMemory, NewSQLite, NewPostgres)
// satisfy this interface.
type RunLister interface {
	// ListRuns returns one RunSummary per run present in the log,
	// ordered by StartedAt descending (newest first). An empty log
	// returns (nil, nil).
	ListRuns(ctx context.Context) ([]RunSummary, error)
}

// ErrLogClosed is returned by any operation on a closed EventLog.
var ErrLogClosed = errors.New("eventlog: log is closed")

// ErrInvalidAppend is the base sentinel wrapped by every append-validation
// failure (wrong Seq, mismatched PrevHash, non-empty PrevHash on first event,
// etc.). Callers can test with errors.Is(err, ErrInvalidAppend).
var ErrInvalidAppend = errors.New("eventlog: invalid append")

// ErrLogCorrupt is the base sentinel wrapped by every Validate failure.
// Callers can test with errors.Is(err, ErrLogCorrupt). Re-exported from
// the root starling package as starling.ErrLogCorrupt.
var ErrLogCorrupt = errors.New("eventlog: log failed validation")

// ErrReadOnly is returned by Append on a log opened in read-only mode
// (WithReadOnly on SQLite, WithReadOnlyPG on Postgres). Lifted from
// the sqlite backend so both durable backends share the sentinel.
var ErrReadOnly = errors.New("eventlog: log is read-only")
