// Package eventlog defines the EventLog interface and ships the default
// backends: in-memory, SQLite, and Postgres.
package eventlog

import (
	"context"
	"errors"

	"github.com/jerkeyray/starling/event"
)

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
	Stream(ctx context.Context, runID string) (<-chan event.Event, error)

	// Close releases all resources and closes every live subscriber
	// channel. Idempotent.
	Close() error
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
