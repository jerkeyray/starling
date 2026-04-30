package eventlog

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
)

// streamBufferSize is the capacity of each subscriber channel. A subscriber
// that falls this many events behind is dropped (its channel is closed).
const streamBufferSize = 256

// subscriber wraps a fan-out channel with sync.Once-guarded close so
// the slow-consumer drop path in Append, the cancel-watcher goroutine,
// and Close all converge to a single close.
type subscriber struct {
	ch    chan event.Event
	once  sync.Once
	runID string
}

func newSubscriber(runID string) *subscriber {
	return &subscriber{ch: make(chan event.Event, streamBufferSize), runID: runID}
}

func (s *subscriber) close() { s.once.Do(func() { close(s.ch) }) }

// detach removes sub from the run's subscriber list (if present) and
// closes it. Idempotent against the slow-consumer drop path in Append.
func detach(sub *subscriber, m *memoryLog) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	rs, ok := m.runs[sub.runID]
	if ok {
		for i, s := range rs.subscribers {
			if s == sub {
				rs.subscribers = append(rs.subscribers[:i], rs.subscribers[i+1:]...)
				break
			}
		}
	}
	sub.close()
}

// NewInMemory returns an in-memory EventLog suitable for tests, demos, and
// single-process embedded use. It is safe for concurrent use across runs.
func NewInMemory() EventLog {
	return &memoryLog{
		runs: make(map[string]*runState),
	}
}

type memoryLog struct {
	mu     sync.RWMutex
	runs   map[string]*runState
	closed bool
}

type runState struct {
	events      []event.Event
	subscribers []*subscriber
}

func (m *memoryLog) Append(ctx context.Context, runID string, ev event.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrLogClosed
	}

	rs, ok := m.runs[runID]
	if !ok {
		rs = &runState{}
		m.runs[runID] = rs
	}

	var last *event.Event
	if n := len(rs.events); n > 0 {
		last = &rs.events[n-1]
	}
	if err := validateAppend(runID, last, ev); err != nil {
		return err
	}

	rs.events = append(rs.events, ev)

	// Non-blocking fan-out; drop slow subscribers.
	kept := rs.subscribers[:0]
	for _, sub := range rs.subscribers {
		select {
		case sub.ch <- ev:
			kept = append(kept, sub)
		default:
			sub.close()
			// Dropped slow subscribers are a safety-critical signal
			// and are always logged via slog.Default(), regardless of
			// Config.Logger. The event log runs decoupled from any
			// agent's run-scoped logger. Documented in Config.Logger
			// godoc.
			slog.Default().LogAttrs(ctx, slog.LevelWarn,
				"eventlog: dropped slow stream subscriber",
				slog.String("run_id", runID),
				slog.Int("buffer_size", cap(sub.ch)),
			)
		}
	}
	rs.subscribers = kept

	return nil
}

// validateAppend checks that ev is a valid next event after last.
// A nil last means ev must be the first event of a run. Shared
// between the in-memory, SQLite, and Postgres backends so all three
// enforce identical chain invariants.
//
// runID is the run key the caller addressed; ev.RunID is what the
// event carries. Memory keys storage by the parameter while SQL
// backends bind ev.RunID, so a mismatch silently splits a run's
// history; we reject up front.
func validateAppend(runID string, last *event.Event, ev event.Event) error {
	if runID == "" {
		return fmt.Errorf("%w: empty runID parameter", ErrInvalidAppend)
	}
	if ev.RunID == "" {
		return fmt.Errorf("%w: event has empty RunID", ErrInvalidAppend)
	}
	if runID != ev.RunID {
		return fmt.Errorf("%w: runID parameter %q does not match event RunID %q", ErrInvalidAppend, runID, ev.RunID)
	}
	if last == nil {
		if ev.Seq != 1 {
			return fmt.Errorf("%w: first event must have Seq=1, got %d", ErrInvalidAppend, ev.Seq)
		}
		if len(ev.PrevHash) != 0 {
			return fmt.Errorf("%w: first event must have empty PrevHash", ErrInvalidAppend)
		}
		return nil
	}

	if ev.Seq != last.Seq+1 {
		return fmt.Errorf("%w: expected Seq=%d, got %d", ErrInvalidAppend, last.Seq+1, ev.Seq)
	}

	lastBytes, err := event.Marshal(*last)
	if err != nil {
		return fmt.Errorf("%w: re-marshal previous event: %v", ErrInvalidAppend, err)
	}
	want := event.Hash(lastBytes)
	if !bytes.Equal(ev.PrevHash, want) {
		return fmt.Errorf("%w: PrevHash does not match prior event hash", ErrInvalidAppend)
	}
	return nil
}

func (m *memoryLog) Read(ctx context.Context, runID string) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, ErrLogClosed
	}

	rs, ok := m.runs[runID]
	if !ok {
		return nil, nil
	}

	out := make([]event.Event, len(rs.events))
	copy(out, rs.events)
	return out, nil
}

func (m *memoryLog) Stream(ctx context.Context, runID string) (<-chan event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrLogClosed
	}

	rs, ok := m.runs[runID]
	if !ok {
		rs = &runState{}
		m.runs[runID] = rs
	}

	sub := newSubscriber(runID)

	// Subscribe-then-deliver-history under the lock so Appends
	// issued after Stream returns reach the channel. If history fits
	// in the buffer, deliver inline; otherwise pump from a goroutine.
	history := make([]event.Event, len(rs.events))
	copy(history, rs.events)

	if len(history) <= cap(sub.ch) {
		for _, ev := range history {
			sub.ch <- ev
		}
	}
	rs.subscribers = append(rs.subscribers, sub)
	m.mu.Unlock()

	go func() {
		<-ctx.Done()
		detach(sub, m)
	}()

	if len(history) > cap(sub.ch) {
		go func() {
			for _, ev := range history {
				select {
				case sub.ch <- ev:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	return sub.ch, nil
}

func (m *memoryLog) ListRuns(ctx context.Context) ([]RunSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, ErrLogClosed
	}

	out := make([]RunSummary, 0, len(m.runs))
	for runID, rs := range m.runs {
		if len(rs.events) == 0 {
			continue
		}
		first := rs.events[0]
		last := rs.events[len(rs.events)-1]
		out = append(out, RunSummary{
			RunID:        runID,
			StartedAt:    time.Unix(0, first.Timestamp),
			LastSeq:      last.Seq,
			TerminalKind: last.Kind,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

func (m *memoryLog) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	for _, rs := range m.runs {
		for _, sub := range rs.subscribers {
			sub.close()
		}
		rs.subscribers = nil
	}
	return nil
}
