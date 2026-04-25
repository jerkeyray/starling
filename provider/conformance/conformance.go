package conformance

// Reusable conformance suite every provider adapter must pass.
// Adapters call Run from a TestConformance test in their package.

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/provider"
)

type Capabilities = provider.Capabilities

// Scenario names a canned interaction the Adapter must arrange.
type Scenario int

const (
	// ScenarioTextOnly: short text reply, then usage and ChunkEnd.
	ScenarioTextOnly Scenario = iota + 1

	// ScenarioToolCall: one tool_use block with CallID="call-1",
	// Name="search", Args={"q":"go"}, then usage and ChunkEnd.
	ScenarioToolCall

	// ScenarioStreamError: vendor-side error mid-stream. Must surface
	// as a non-EOF error from Next.
	ScenarioStreamError
)

type Adapter interface {
	Name() string
	Capabilities() Capabilities
	NewProvider(t *testing.T, s Scenario) provider.Provider
}

func Run(t *testing.T, a Adapter) {
	t.Helper()
	t.Run(a.Name()+"/TextOnly", func(t *testing.T) { runTextOnly(t, a) })
	if a.Capabilities().Tools {
		t.Run(a.Name()+"/ToolCall", func(t *testing.T) { runToolCall(t, a) })
	}
	t.Run(a.Name()+"/StreamError", func(t *testing.T) { runStreamError(t, a) })
	t.Run(a.Name()+"/Cancellation", func(t *testing.T) { runCancellation(t, a) })
}

func drain(t *testing.T, s provider.EventStream) ([]provider.StreamChunk, error) {
	t.Helper()
	var out []provider.StreamChunk
	ctx := context.Background()
	for {
		c, err := s.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
}

func runTextOnly(t *testing.T, a Adapter) {
	t.Helper()
	p := a.NewProvider(t, ScenarioTextOnly)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	chunks, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	assertEndsWithChunkEnd(t, chunks)
	assertExactlyOneChunkEnd(t, chunks)
	assertHasUsage(t, chunks)
	if a.Capabilities().RequestID {
		end := lastEnd(chunks)
		if end.ProviderReqID == "" {
			t.Error("ChunkEnd.ProviderReqID is empty; capability declares RequestID=true")
		}
	}
	end := lastEnd(chunks)
	if len(end.RawResponseHash) != 0 && len(end.RawResponseHash) != event.HashSize {
		t.Errorf("ChunkEnd.RawResponseHash len = %d, want 0 or %d", len(end.RawResponseHash), event.HashSize)
	}
}

func runToolCall(t *testing.T, a Adapter) {
	t.Helper()
	p := a.NewProvider(t, ScenarioToolCall)
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model: "test-model",
		Tools: []provider.ToolDefinition{
			{Name: "search", Description: "search the web", Schema: []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	chunks, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	assertEndsWithChunkEnd(t, chunks)
	assertExactlyOneChunkEnd(t, chunks)

	// Tool blocks must be well-formed: a Start, zero or more Deltas
	// for the same CallID, then an End.
	open := map[string]bool{}
	closed := map[string]bool{}
	for _, c := range chunks {
		switch c.Kind {
		case provider.ChunkToolUseStart:
			if c.ToolUse == nil || c.ToolUse.CallID == "" {
				t.Fatalf("ChunkToolUseStart with nil/empty CallID")
			}
			open[c.ToolUse.CallID] = true
		case provider.ChunkToolUseDelta:
			if c.ToolUse == nil || !open[c.ToolUse.CallID] {
				t.Fatalf("ChunkToolUseDelta without prior Start: %+v", c)
			}
		case provider.ChunkToolUseEnd:
			if c.ToolUse == nil || !open[c.ToolUse.CallID] {
				t.Fatalf("ChunkToolUseEnd without prior Start: %+v", c)
			}
			closed[c.ToolUse.CallID] = true
		}
	}
	for id := range open {
		if !closed[id] {
			t.Errorf("CallID %q opened but never closed", id)
		}
	}
	if len(open) == 0 {
		t.Error("scenario produced no ChunkToolUseStart")
	}
}

func runStreamError(t *testing.T, a Adapter) {
	t.Helper()
	p := a.NewProvider(t, ScenarioStreamError)
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "test-model"})
	if err != nil {
		// Some adapters reject up front; that's acceptable.
		return
	}
	defer stream.Close()
	_, drainErr := drain(t, stream)
	if drainErr == nil {
		t.Fatal("ScenarioStreamError produced no error; want non-nil from Next")
	}
	if errors.Is(drainErr, io.EOF) {
		t.Fatalf("ScenarioStreamError surfaced as io.EOF; adapter must classify as a real error")
	}
}

func runCancellation(t *testing.T, a Adapter) {
	t.Helper()
	p := a.NewProvider(t, ScenarioTextOnly)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.Stream(ctx, &provider.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	cancel()
	for {
		_, nerr := stream.Next(ctx)
		if nerr == nil {
			continue
		}
		if errors.Is(nerr, io.EOF) {
			// Some adapters finish quickly enough that EOF beats the
			// cancellation. Acceptable as long as no chunks-after-end
			// or panic occurred.
			return
		}
		// Any non-EOF error is the cancellation surfacing. Acceptable.
		return
	}
}

func assertEndsWithChunkEnd(t *testing.T, chunks []provider.StreamChunk) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("no chunks received")
	}
	if chunks[len(chunks)-1].Kind != provider.ChunkEnd {
		t.Fatalf("last chunk kind = %s, want ChunkEnd", chunks[len(chunks)-1].Kind)
	}
}

func assertExactlyOneChunkEnd(t *testing.T, chunks []provider.StreamChunk) {
	t.Helper()
	count := 0
	for _, c := range chunks {
		if c.Kind == provider.ChunkEnd {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ChunkEnd count = %d, want exactly 1", count)
	}
}

func assertHasUsage(t *testing.T, chunks []provider.StreamChunk) {
	t.Helper()
	for _, c := range chunks {
		if c.Kind == provider.ChunkUsage && c.Usage != nil {
			return
		}
	}
	t.Error("no ChunkUsage emitted; usage reporting is required")
}

func lastEnd(chunks []provider.StreamChunk) provider.StreamChunk {
	for i := len(chunks) - 1; i >= 0; i-- {
		if chunks[i].Kind == provider.ChunkEnd {
			return chunks[i]
		}
	}
	return provider.StreamChunk{}
}
