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

// RunSummary is a per-run capsule returned by EventLog.ListRuns —
// enough to populate a list-of-runs view without loading every event.
// TerminalKind is the most recent event's kind; for an in-progress run
// it won't satisfy Kind.IsTerminal.
//
// Aggregates (TurnCount, CostUSD, ...) are derived from the run's
// AssistantMessageCompleted and ToolCallScheduled events. They are
// best-effort: a backend may return zeros for in-progress runs whose
// totals haven't been recomputed since the last append. For a
// guaranteed point-in-time snapshot, call Read and aggregate.
type RunSummary struct {
	RunID        string
	StartedAt    time.Time
	LastSeq      uint64
	TerminalKind event.Kind

	// Aggregates over the run's events. Zero values are valid for runs
	// that haven't produced an AssistantMessageCompleted yet.
	TurnCount     int
	ToolCallCount int
	InputTokens   int64
	OutputTokens  int64
	CostUSD       float64
	DurationMs    int64 // wall time from RunStarted to last event; 0 while still running's first event
}

// RunPageOptions filters and pages a run listing. Limit <= 0 means
// no limit; Offset < 0 is treated as 0 by implementations.
type RunPageOptions struct {
	Limit            int
	Offset           int
	Status           string
	Query            string
	StartedAfter     time.Time
	RequireToolCalls bool
}

// RunPage is the result of a paged run listing. TotalMatching is the
// number of runs matching the filters before Limit/Offset are applied.
type RunPage struct {
	Runs          []RunSummary
	TotalMatching int
	Limit         int
	Offset        int
}

// PruneOptions selects old runs for deletion. Retention/pruning is not
// part of the append-only EventLog contract; it is an explicit
// operator action for logs whose retention window has expired.
type PruneOptions struct {
	// Before deletes runs whose StartedAt is strictly before this time.
	// It must be non-zero.
	Before time.Time

	// Status optionally narrows deletion to "completed", "failed",
	// "cancelled", or "in progress". Empty means all terminal runs:
	// completed, failed, and cancelled.
	Status string

	// IncludeInProgress permits pruning in-progress runs when Status is
	// empty. It is ignored when Status is set. Leave false for normal
	// retention jobs.
	IncludeInProgress bool

	// Limit caps the number of runs pruned in one call. Limit <= 0 means
	// no cap.
	Limit int

	// DryRun reports what would be removed without deleting anything.
	DryRun bool
}

// PruneReport summarizes a retention pass.
type PruneReport struct {
	MatchedRuns   int
	DeletedRuns   int
	MatchedEvents int
	DeletedEvents int
	DryRun        bool
	RunIDs        []string
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
	// runID and ev.RunID must both be non-empty and equal: backends
	// disagree on which they index by (memory uses the parameter, SQL
	// backends bind ev.RunID), so mismatched values are rejected up
	// front. Returns an error wrapping ErrInvalidAppend on invariant
	// violation, or ErrLogClosed if the log has been closed.
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

// RunPageLister is implemented by backends that can filter and page
// run listings before materializing every run summary. Inspector uses
// this when available and falls back to RunLister for custom backends.
type RunPageLister interface {
	ListRunsPage(ctx context.Context, opts RunPageOptions) (RunPage, error)
}

// RunPruner is implemented by backends that support explicit retention
// cleanup. Pruning deletes whole runs only; it never removes a suffix of
// a run, which would invalidate the recorded hash chain.
type RunPruner interface {
	PruneRuns(ctx context.Context, opts PruneOptions) (PruneReport, error)
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
