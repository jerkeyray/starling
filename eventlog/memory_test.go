package eventlog_test

// In-memory-specific tests. Every test that isn't tied to memory's
// implementation details (slow-consumer drop, the long-history fix,
// the tight-loop concurrency stress) lives in contract_test.go and
// runs against every backend.

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"golang.org/x/sync/errgroup"
)

// TestStream_LongHistoryNotTruncated guards against a regression where
// runs longer than streamBufferSize were silently truncated to the
// first N events and the channel closed. The replay/inspector use
// case is exactly long-run streaming, so this must deliver every event.
//
// Memory-specific because it targets the in-memory backend's goroutine-
// pump path for histories larger than the channel buffer. SQLite's
// Stream is a fresh poll every tick and can't hit this bug.
func TestStream_LongHistoryNotTruncated(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const total = 600 // > streamBufferSize (256)
	var cb chainBuilder
	for i := 0; i < total; i++ {
		ev := cb.next(t, "r1", fmt.Sprintf("hist-%d", i))
		if err := log.Append(ctx, "r1", ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	ch, err := log.Stream(ctx, "r1")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	got := make([]uint64, 0, total)
	timeout := time.After(5 * time.Second)
	for len(got) < total {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d/%d events", len(got), total)
			}
			got = append(got, ev.Seq)
		case <-timeout:
			t.Fatalf("timeout: only got %d/%d events", len(got), total)
		}
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("got[%d]: Seq=%d, want %d", i, seq, i+1)
		}
	}
}

// TestStream_SlowConsumerGetsClosed pins memory's drop-slow-consumer
// policy: a subscriber that falls more than streamBufferSize events
// behind has its channel closed. Durable backends do not share this
// semantic (SQLite polls; a slow consumer just blocks on the channel
// until ctx is cancelled), so the test stays memory-specific.
func TestStream_SlowConsumerGetsClosed(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx := context.Background()

	ch, err := log.Stream(ctx, "r1")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var cb chainBuilder
	// Append enough events to overflow the 256-capacity buffer.
	for i := 0; i < 300; i++ {
		if err := log.Append(ctx, "r1", cb.next(t, "r1", "x")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("channel not closed after overflow")
		}
	}
}

// TestConcurrent_100AppendsAcross10Runs hammers the log with
// concurrent Appends on disjoint runs. Memory-specific in commit 1:
// SQLite's single-writer BeginTx model has not been tested under this
// kind of load, and Postgres gets its own dedicated concurrency test
// (exercising the per-run advisory lock) alongside the Postgres
// backend.
func TestConcurrent_100AppendsAcross10Runs(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx := context.Background()

	const runs = 10
	const perRun = 10

	g, gctx := errgroup.WithContext(ctx)
	var wg sync.WaitGroup
	for r := 0; r < runs; r++ {
		r := r
		wg.Add(1)
		g.Go(func() error {
			defer wg.Done()
			runID := fmt.Sprintf("run-%d", r)
			var cb chainBuilder
			for i := 0; i < perRun; i++ {
				if err := log.Append(gctx, runID, cb.next(t, runID, fmt.Sprintf("%d-%d", r, i))); err != nil {
					return fmt.Errorf("run %d append %d: %w", r, i, err)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("concurrent appends: %v", err)
	}

	for r := 0; r < runs; r++ {
		runID := fmt.Sprintf("run-%d", r)
		events, err := log.Read(ctx, runID)
		if err != nil {
			t.Fatalf("Read %s: %v", runID, err)
		}
		if len(events) != perRun {
			t.Fatalf("run %s: want %d events, got %d", runID, perRun, len(events))
		}
		for i, ev := range events {
			if ev.Seq != uint64(i+1) {
				t.Fatalf("run %s: event %d has Seq=%d, want %d", runID, i, ev.Seq, i+1)
			}
			if i == 0 {
				if len(ev.PrevHash) != 0 {
					t.Fatalf("run %s: first event has non-empty PrevHash", runID)
				}
				continue
			}
			prevBytes, err := event.Marshal(events[i-1])
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			want := event.Hash(prevBytes)
			if !reflect.DeepEqual(ev.PrevHash, want) {
				t.Fatalf("run %s: event %d PrevHash mismatch", runID, i)
			}
		}
	}
}
