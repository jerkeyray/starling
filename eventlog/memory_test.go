package eventlog_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"golang.org/x/sync/errgroup"
)

// chainBuilder tracks per-run Seq/PrevHash state so tests don't reimplement
// the chain arithmetic.
type chainBuilder struct {
	seq      uint64
	prevHash []byte
}

// next returns a fresh Event with valid Seq and PrevHash. The payload is a
// UserMessageAppended carrying the supplied content so events are distinct.
func (cb *chainBuilder) next(t *testing.T, runID, content string) event.Event {
	t.Helper()
	payload, err := event.EncodePayload(event.UserMessageAppended{Content: content})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	cb.seq++
	ev := event.Event{
		RunID:     runID,
		Seq:       cb.seq,
		PrevHash:  cb.prevHash,
		Timestamp: int64(cb.seq) * 1_000_000,
		Kind:      event.KindUserMessageAppended,
		Payload:   payload,
	}
	encoded, err := event.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cb.prevHash = event.Hash(encoded)
	return ev
}

func TestAppend_FirstEventMustStartAtSeq1(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()

	payload, _ := event.EncodePayload(event.UserMessageAppended{Content: "x"})
	ev := event.Event{RunID: "r1", Seq: 5, Kind: event.KindUserMessageAppended, Payload: payload}
	err := log.Append(context.Background(), "r1", ev)
	if !errors.Is(err, eventlog.ErrInvalidAppend) {
		t.Fatalf("want ErrInvalidAppend, got %v", err)
	}
}

func TestAppend_FirstEventMustHaveEmptyPrevHash(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()

	payload, _ := event.EncodePayload(event.UserMessageAppended{Content: "x"})
	ev := event.Event{
		RunID: "r1", Seq: 1, PrevHash: []byte{0x01},
		Kind: event.KindUserMessageAppended, Payload: payload,
	}
	err := log.Append(context.Background(), "r1", ev)
	if !errors.Is(err, eventlog.ErrInvalidAppend) {
		t.Fatalf("want ErrInvalidAppend, got %v", err)
	}
}

func TestAppend_SeqMustBeMonotonic(t *testing.T) {
	ctx := context.Background()

	t.Run("skip", func(t *testing.T) {
		log := eventlog.NewInMemory()
		defer log.Close()
		var cb chainBuilder
		if err := log.Append(ctx, "r1", cb.next(t, "r1", "a")); err != nil {
			t.Fatalf("first append: %v", err)
		}
		// Build a legitimate second event, then bump its Seq to skip.
		bad := cb.next(t, "r1", "b")
		bad.Seq = 3
		if err := log.Append(ctx, "r1", bad); !errors.Is(err, eventlog.ErrInvalidAppend) {
			t.Fatalf("want ErrInvalidAppend, got %v", err)
		}
	})

	t.Run("replay", func(t *testing.T) {
		log := eventlog.NewInMemory()
		defer log.Close()
		var cb chainBuilder
		first := cb.next(t, "r1", "a")
		if err := log.Append(ctx, "r1", first); err != nil {
			t.Fatalf("first append: %v", err)
		}
		if err := log.Append(ctx, "r1", first); !errors.Is(err, eventlog.ErrInvalidAppend) {
			t.Fatalf("want ErrInvalidAppend, got %v", err)
		}
	})
}

func TestAppend_PrevHashMustMatchPrevEvent(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx := context.Background()

	var cb chainBuilder
	if err := log.Append(ctx, "r1", cb.next(t, "r1", "a")); err != nil {
		t.Fatalf("first append: %v", err)
	}
	bad := cb.next(t, "r1", "b")
	bad.PrevHash = make([]byte, event.HashSize) // all zeroes, wrong
	if err := log.Append(ctx, "r1", bad); !errors.Is(err, eventlog.ErrInvalidAppend) {
		t.Fatalf("want ErrInvalidAppend, got %v", err)
	}
}

func TestAppend_HappyPath(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx := context.Background()

	var cb chainBuilder
	want := make([]event.Event, 5)
	for i := 0; i < 5; i++ {
		ev := cb.next(t, "r1", fmt.Sprintf("msg-%d", i))
		want[i] = ev
		if err := log.Append(ctx, "r1", ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := log.Read(ctx, "r1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\nwant=%+v\ngot =%+v", want, got)
	}
}

func TestRead_UnknownRunID_EmptySlice(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()

	got, err := log.Read(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil slice, got %v", got)
	}
}

func TestRead_ReturnsDefensiveCopy(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx := context.Background()

	var cb chainBuilder
	for i := 0; i < 3; i++ {
		if err := log.Append(ctx, "r1", cb.next(t, "r1", "x")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	snap, err := log.Read(ctx, "r1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Mutate the returned slice.
	snap[0].RunID = "TAMPERED"

	again, err := log.Read(ctx, "r1")
	if err != nil {
		t.Fatalf("Read again: %v", err)
	}
	if again[0].RunID != "r1" {
		t.Fatalf("internal state mutated via returned slice: got RunID=%q", again[0].RunID)
	}
}

func TestStream_ReplayThenLive(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cb chainBuilder
	historical := make([]event.Event, 3)
	for i := 0; i < 3; i++ {
		historical[i] = cb.next(t, "r1", fmt.Sprintf("hist-%d", i))
		if err := log.Append(ctx, "r1", historical[i]); err != nil {
			t.Fatalf("append hist %d: %v", i, err)
		}
	}

	ch, err := log.Stream(ctx, "r1")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Drain historical.
	for i := 0; i < 3; i++ {
		select {
		case got := <-ch:
			if got.Seq != historical[i].Seq {
				t.Fatalf("hist[%d]: want Seq=%d, got %d", i, historical[i].Seq, got.Seq)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout draining historical[%d]", i)
		}
	}

	// Append live.
	live := make([]event.Event, 2)
	for i := 0; i < 2; i++ {
		live[i] = cb.next(t, "r1", fmt.Sprintf("live-%d", i))
		if err := log.Append(ctx, "r1", live[i]); err != nil {
			t.Fatalf("append live %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case got := <-ch:
			if got.Seq != live[i].Seq {
				t.Fatalf("live[%d]: want Seq=%d, got %d", i, live[i].Seq, got.Seq)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout draining live[%d]", i)
		}
	}
}

// TestStream_LongHistoryNotTruncated guards against a regression where
// runs longer than streamBufferSize were silently truncated to the
// first N events and the channel closed. The replay/inspector use
// case is exactly long-run streaming, so this must deliver every event.
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

func TestStream_ContextCancelClosesChannel(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := log.Stream(ctx, "r1")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("channel should be closed after cancel")
		}
	case <-time.After(time.Second):
		t.Fatalf("channel not closed within timeout")
	}
}

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

	// Drain what's buffered; eventually channel closes.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed as expected
			}
		case <-deadline:
			t.Fatalf("channel not closed after overflow")
		}
	}
}

func TestClose_BlocksFurtherAppend(t *testing.T) {
	log := eventlog.NewInMemory()
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var cb chainBuilder
	err := log.Append(context.Background(), "r1", cb.next(t, "r1", "x"))
	if !errors.Is(err, eventlog.ErrLogClosed) {
		t.Fatalf("want ErrLogClosed, got %v", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	log := eventlog.NewInMemory()
	if err := log.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestConcurrent_100AppendsAcross10Runs(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	ctx := context.Background()

	const runs = 10
	const perRun = 10

	g, gctx := errgroup.WithContext(ctx)
	// Guard against NewInMemory returning a shared map: each run has its
	// own chainBuilder, but we're hammering the same log from N goroutines.
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

	// Verify every run's chain is intact.
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
