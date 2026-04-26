package bench

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

func BenchmarkAppend_InMemory(b *testing.B) {
	log := eventlog.NewInMemory()
	b.Cleanup(func() { _ = log.Close() })
	benchAppend(b, log, "r1")
}

func BenchmarkAppend_SQLite(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		b.Fatalf("NewSQLite: %v", err)
	}
	b.Cleanup(func() { _ = log.Close() })
	benchAppend(b, log, "r1")
}

func benchAppend(b *testing.B, log eventlog.EventLog, runID string) {
	b.Helper()
	ctx := context.Background()
	cb := newChain()

	if err := log.Append(ctx, runID, cb.runStarted(b, runID)); err != nil {
		b.Fatalf("seed Append: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := log.Append(ctx, runID, cb.userMsg(b, runID, "msg")); err != nil {
			b.Fatalf("Append %d: %v", i, err)
		}
	}
	b.ReportMetric(float64(b.N), "events")
}

// chain mints chained events for a single run.
type chain struct {
	seq      uint64
	prevHash []byte
}

func newChain() *chain { return &chain{} }

func (c *chain) runStarted(b *testing.B, runID string) event.Event {
	b.Helper()
	c.seq++
	payload, err := event.EncodePayload(event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          "bench",
		ProviderID:    "bench",
		ModelID:       "bench-model",
	})
	if err != nil {
		b.Fatalf("EncodePayload: %v", err)
	}
	ev := event.Event{
		RunID:     runID,
		Seq:       c.seq,
		Timestamp: int64(c.seq),
		Kind:      event.KindRunStarted,
		Payload:   payload,
	}
	c.prevHash = hash(b, ev)
	return ev
}

func (c *chain) userMsg(b *testing.B, runID, content string) event.Event {
	b.Helper()
	c.seq++
	payload, err := event.EncodePayload(event.UserMessageAppended{Content: content})
	if err != nil {
		b.Fatalf("EncodePayload: %v", err)
	}
	ev := event.Event{
		RunID:     runID,
		Seq:       c.seq,
		PrevHash:  c.prevHash,
		Timestamp: int64(c.seq),
		Kind:      event.KindUserMessageAppended,
		Payload:   payload,
	}
	c.prevHash = hash(b, ev)
	return ev
}

func hash(b *testing.B, ev event.Event) []byte {
	b.Helper()
	enc, err := event.Marshal(ev)
	if err != nil {
		b.Fatalf("Marshal: %v", err)
	}
	return event.Hash(enc)
}
