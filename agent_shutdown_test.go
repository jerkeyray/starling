package starling_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/tool"
)

// TestAgent_Shutdown_CancelMidStream covers ctx cancellation while the
// provider is mid-stream: the LLMCall must unblock, and the run must
// terminate with a RunCancelled event written to the log (the event
// log write itself is meant to be cancellation-immune via
// context.WithoutCancel at the emit boundary).
func TestAgent_Shutdown_CancelMidStream(t *testing.T) {
	p := &cancellableProvider{started: make(chan struct{})}
	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "m", MaxTurns: 2},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *starling.RunResult, 1)
	errc := make(chan error, 1)
	go func() {
		res, err := a.Run(ctx, "go")
		done <- res
		errc <- err
	}()

	// Wait until the provider stream is running, then cancel.
	<-p.started
	cancel()

	var res *starling.RunResult
	var err error
	select {
	case res = <-done:
		err = <-errc
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res == nil || res.RunID == "" {
		t.Fatalf("nil result")
	}
	evs, _ := log.Read(context.Background(), res.RunID)
	if len(evs) == 0 || evs[len(evs)-1].Kind != event.KindRunCancelled {
		t.Fatalf("last event = %v, want RunCancelled", evs[len(evs)-1].Kind)
	}
}

// TestAgent_Shutdown_CancelMidTool cancels while a tool is executing.
// The tool's ctx inherits the run ctx, so its blocking work must
// unblock and the run must terminate as RunCancelled.
func TestAgent_Shutdown_CancelMidTool(t *testing.T) {
	p := &cannedProvider{scripts: [][]provider.StreamChunk{{
		{Kind: provider.ChunkToolUseStart, ToolUse: &provider.ToolUseChunk{CallID: "c1", Name: "sleep"}},
		{Kind: provider.ChunkToolUseDelta, ToolUse: &provider.ToolUseChunk{CallID: "c1", ArgsDelta: `{}`}},
		{Kind: provider.ChunkToolUseEnd, ToolUse: &provider.ToolUseChunk{CallID: "c1"}},
		{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 5, OutputTokens: 2}},
		{Kind: provider.ChunkEnd, StopReason: "tool_use"},
	}}}

	started := make(chan struct{}, 1)
	sleepTool := tool.Typed("sleep", "",
		func(ctx context.Context, _ struct{}) (struct{}, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return struct{}{}, ctx.Err()
		})

	log := eventlog.NewInMemory()
	defer log.Close()
	a := &starling.Agent{
		Provider: p,
		Tools:    []tool.Tool{sleepTool},
		Log:      log,
		Config:   starling.Config{Model: "m", MaxTurns: 2},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var res *starling.RunResult
	go func() {
		r, err := a.Run(ctx, "go")
		res = r
		done <- err
	}()

	<-started
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	evs, _ := log.Read(context.Background(), res.RunID)
	if len(evs) == 0 || evs[len(evs)-1].Kind != event.KindRunCancelled {
		t.Fatalf("last event = %v, want RunCancelled", evs[len(evs)-1].Kind)
	}
}

// TestAgent_Shutdown_TerminalLogAppendErrorSurfaces pins the one case
// where the terminal event can be lost: the log backend rejects the
// terminal append. The error must surface to the caller (not be
// swallowed) so the operator knows the audit trail is incomplete.
func TestAgent_Shutdown_TerminalLogAppendErrorSurfaces(t *testing.T) {
	// A log that accepts the first few events, then rejects everything.
	// The threshold of 2 lets RunStarted + TurnStarted through so we
	// exercise a realistic "fails near the end" shape.
	base := eventlog.NewInMemory()
	defer base.Close()
	log := &failingLog{inner: base, failAfter: 2}

	p := &cannedProvider{scripts: [][]provider.StreamChunk{{
		{Kind: provider.ChunkText, Text: "hi"},
		{Kind: provider.ChunkUsage, Usage: &provider.UsageUpdate{InputTokens: 1, OutputTokens: 1}},
		{Kind: provider.ChunkEnd, StopReason: "stop"},
	}}}
	a := &starling.Agent{
		Provider: p,
		Log:      log,
		Config:   starling.Config{Model: "m", MaxTurns: 2},
	}
	_, err := a.Run(context.Background(), "go")
	if err == nil {
		t.Fatal("expected error when terminal append fails")
	}
}

// ---- helpers -------------------------------------------------------------

// cancellableProvider signals `started` once Stream returns, then
// blocks on ctx inside Next — the simplest provider that lets a test
// cancel the run after the stream is live.
type cancellableProvider struct {
	once    sync.Once
	started chan struct{}
}

func (p *cancellableProvider) Info() provider.Info {
	return provider.Info{ID: "cancellable", APIVersion: "v0"}
}
func (p *cancellableProvider) Stream(_ context.Context, _ *provider.Request) (provider.EventStream, error) {
	return &cancellableStream{parent: p}, nil
}

type cancellableStream struct{ parent *cancellableProvider }

func (s *cancellableStream) Next(ctx context.Context) (provider.StreamChunk, error) {
	s.parent.once.Do(func() { close(s.parent.started) })
	<-ctx.Done()
	return provider.StreamChunk{}, ctx.Err()
}
func (*cancellableStream) Close() error { return nil }

// failingLog wraps an EventLog and returns ErrTerminalWriteRejected on
// every Append after failAfter successful writes. Read/Stream/Close
// pass through to the inner log.
type failingLog struct {
	inner     eventlog.EventLog
	failAfter int
	mu        sync.Mutex
	count     int
}

var errTerminalWriteRejected = errors.New("failingLog: write rejected")

func (f *failingLog) Append(ctx context.Context, runID string, ev event.Event) error {
	f.mu.Lock()
	f.count++
	n := f.count
	f.mu.Unlock()
	if n > f.failAfter {
		return errTerminalWriteRejected
	}
	return f.inner.Append(ctx, runID, ev)
}
func (f *failingLog) Read(ctx context.Context, runID string) ([]event.Event, error) {
	return f.inner.Read(ctx, runID)
}
func (f *failingLog) Stream(ctx context.Context, runID string) (<-chan event.Event, error) {
	return f.inner.Stream(ctx, runID)
}
func (f *failingLog) ListRuns(ctx context.Context) ([]eventlog.RunSummary, error) {
	return f.inner.ListRuns(ctx)
}
func (f *failingLog) Close() error { return f.inner.Close() }
