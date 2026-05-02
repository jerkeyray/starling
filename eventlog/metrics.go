package eventlog

import (
	"context"
	"time"

	"github.com/jerkeyray/starling/event"
)

// AppendObserver records per-Append samples for an instrumented log.
// Implementations must be concurrency-safe and tolerate being called
// in a tight loop.
type AppendObserver interface {
	ObserveAppend(kind event.Kind, status string, d time.Duration)
}

// WithMetrics wraps log so every Append call reports a sample to obs.
// Read, Stream, ListRuns, Close, and any other interfaces (RunLister,
// migrater) are forwarded through unchanged.
//
// Useful for callers that drive Append directly (outside step.emit) and
// want the same latency histogram coverage the agent loop has.
func WithMetrics(log EventLog, obs AppendObserver) EventLog {
	if log == nil || obs == nil {
		return log
	}
	return &observedLog{EventLog: log, obs: obs}
}

type observedLog struct {
	EventLog
	obs AppendObserver
}

func (o *observedLog) Append(ctx context.Context, runID string, ev event.Event) error {
	start := time.Now()
	err := o.EventLog.Append(ctx, runID, ev)
	status := "ok"
	if err != nil {
		status = "error"
	}
	o.obs.ObserveAppend(ev.Kind, status, time.Since(start))
	return err
}

// ListRuns forwards through to the wrapped log when it implements RunLister.
func (o *observedLog) ListRuns(ctx context.Context) ([]RunSummary, error) {
	if rl, ok := o.EventLog.(RunLister); ok {
		return rl.ListRuns(ctx)
	}
	return nil, nil
}

// ListRunsPage forwards through to the wrapped log when it implements
// RunPageLister, or falls back to ListRuns filtering for older backends.
func (o *observedLog) ListRunsPage(ctx context.Context, opts RunPageOptions) (RunPage, error) {
	if rl, ok := o.EventLog.(RunPageLister); ok {
		return rl.ListRunsPage(ctx, opts)
	}
	runs, err := o.ListRuns(ctx)
	if err != nil {
		return RunPage{}, err
	}
	return paginateRunSummaries(runs, opts), nil
}

func (o *observedLog) currentVersion(ctx context.Context) (int, error) {
	if m, ok := o.EventLog.(migrater); ok {
		return m.currentVersion(ctx)
	}
	return 0, nil
}

func (o *observedLog) migrate(ctx context.Context, dryRun bool) (MigrationReport, error) {
	if m, ok := o.EventLog.(migrater); ok {
		return m.migrate(ctx, dryRun)
	}
	return MigrationReport{}, nil
}

func (o *observedLog) expectedVersion() int {
	if m, ok := o.EventLog.(migrater); ok {
		return m.expectedVersion()
	}
	return 0
}
