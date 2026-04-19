package eventlog

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/jerkeyray/starling/event"
)

// streamBufferSize is the capacity of each subscriber channel. A subscriber
// that falls this many events behind is dropped (its channel is closed).
const streamBufferSize = 256

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
	subscribers []chan event.Event
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
	if err := validateAppend(last, ev); err != nil {
		return err
	}

	rs.events = append(rs.events, ev)

	// Non-blocking fan-out; drop slow subscribers.
	kept := rs.subscribers[:0]
	for _, sub := range rs.subscribers {
		select {
		case sub <- ev:
			kept = append(kept, sub)
		default:
			close(sub)
		}
	}
	rs.subscribers = kept

	return nil
}

// validateAppend checks that ev is a valid next event after last.
// A nil last means ev must be the first event of a run. Shared
// between the in-memory and SQLite backends so both enforce identical
// chain invariants.
func validateAppend(last *event.Event, ev event.Event) error {
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

	ch := make(chan event.Event, streamBufferSize)

	// Historical replay: if more events already exist than the buffer can
	// hold, we can't deliver history + go live on the same channel. Close
	// immediately after draining what fits so the caller sees a clean
	// signal rather than silent data loss.
	if len(rs.events) > streamBufferSize {
		for i := 0; i < streamBufferSize; i++ {
			ch <- rs.events[i]
		}
		close(ch)
		m.mu.Unlock()
		return ch, nil
	}

	for _, ev := range rs.events {
		ch <- ev
	}
	rs.subscribers = append(rs.subscribers, ch)
	m.mu.Unlock()

	// Watch for ctx cancellation; on cancel, remove the subscriber and
	// close the channel (if not already closed by a slow-consumer drop or
	// Close()).
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed {
			// Close() already closed every subscriber channel.
			return
		}
		rs, ok := m.runs[runID]
		if !ok {
			return
		}
		for i, sub := range rs.subscribers {
			if sub == ch {
				rs.subscribers = append(rs.subscribers[:i], rs.subscribers[i+1:]...)
				close(ch)
				return
			}
		}
		// Subscriber already dropped (slow-consumer close) — nothing to do.
	}()

	return ch, nil
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
			close(sub)
		}
		rs.subscribers = nil
	}
	return nil
}
