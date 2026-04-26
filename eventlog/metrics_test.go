package eventlog_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

type recordingObs struct {
	mu      sync.Mutex
	samples []obsSample
}

type obsSample struct {
	Kind   event.Kind
	Status string
}

func (r *recordingObs) ObserveAppend(kind event.Kind, status string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, obsSample{Kind: kind, Status: status})
}

func TestWithMetrics_RecordsAppendSamples(t *testing.T) {
	inner := eventlog.NewInMemory()
	defer inner.Close()
	obs := &recordingObs{}
	wrapped := eventlog.WithMetrics(inner, obs)

	cb := &chainBuilder{}
	if err := wrapped.Append(context.Background(), "r1", cb.next(t, "r1", "hi")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := wrapped.Append(context.Background(), "r1", cb.next(t, "r1", "hi-2")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(obs.samples))
	}
	if obs.samples[0].Status != "ok" {
		t.Errorf("samples[0].Status = %q, want ok", obs.samples[0].Status)
	}
}
