package bench

import (
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/merkle"
)

func BenchmarkValidate_1k(b *testing.B) { benchValidate(b, 1_000) }

func BenchmarkValidate_10k(b *testing.B) { benchValidate(b, 10_000) }

func benchValidate(b *testing.B, n int) {
	b.Helper()
	evs := buildRun(b, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := eventlog.Validate(evs); err != nil {
			b.Fatalf("Validate: %v", err)
		}
	}
	b.ReportMetric(float64(n), "events")
}

func buildRun(b *testing.B, n int) []event.Event {
	b.Helper()
	cb := newChain()
	out := make([]event.Event, 0, n+1)

	out = append(out, cb.runStarted(b, "r1"))
	for i := 1; i < n; i++ {
		out = append(out, cb.userMsg(b, "r1", "msg"))
	}

	leaves, err := merkle.EventHashes(out)
	if err != nil {
		b.Fatalf("merkle.EventHashes: %v", err)
	}
	root := merkle.Root(leaves)
	out = append(out, cb.runCompleted(b, "r1", root))
	return out
}

func (c *chain) runCompleted(b *testing.B, runID string, root []byte) event.Event {
	b.Helper()
	c.seq++
	payload, err := event.EncodePayload(event.RunCompleted{
		FinalText:  "ok",
		MerkleRoot: root,
	})
	if err != nil {
		b.Fatalf("EncodePayload: %v", err)
	}
	ev := event.Event{
		RunID:     runID,
		Seq:       c.seq,
		PrevHash:  c.prevHash,
		Timestamp: int64(c.seq),
		Kind:      event.KindRunCompleted,
		Payload:   payload,
	}
	c.prevHash = hash(b, ev)
	return ev
}
