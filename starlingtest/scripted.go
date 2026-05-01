// Package starlingtest exposes test helpers for downstream consumers
// of Starling: a deterministic scripted Provider, in-memory event-log
// seeders, and replay assertions. The surface mirrors helpers that
// have been duplicated across the runtime's own test files; the goal
// is for external test suites to reach for the same primitives
// without copy-pasting.
//
// Import only from _test.go files. The package depends on the
// "testing" package and is not intended for production code.
package starlingtest

import (
	"context"
	"errors"
	"io"

	"github.com/jerkeyray/starling/provider"
)

// ScriptedProvider returns the next pre-built sequence of StreamChunks
// on every call to Stream. It is deterministic, has no network, and
// is safe to use across replay round-trips because subsequent Stream
// calls advance through Scripts in order.
//
// A typical pattern: one element per turn, each element a slice of
// chunks the agent should observe within that turn. After the last
// script is consumed the next Stream call returns ErrScriptsExhausted.
type ScriptedProvider struct {
	// Scripts is the per-Stream-call sequence of chunks. Mandatory;
	// an empty Scripts produces ErrScriptsExhausted on the first call.
	Scripts [][]provider.StreamChunk

	// ProviderInfo is returned from Info(). Zero value defaults to
	// {ID:"scripted", APIVersion:"v0"}.
	ProviderInfo provider.Info

	// OpenErr, if non-nil, is returned from Stream() instead of a
	// scripted stream. Useful for testing provider-failure paths.
	OpenErr error

	// idx is the next script index to serve. Not exported; tests that
	// want to inspect progress should count Stream calls themselves.
	idx int
}

// ErrScriptsExhausted is returned from Stream once every entry in
// Scripts has been served.
var ErrScriptsExhausted = errors.New("starlingtest: scripted provider exhausted")

// Info implements provider.Provider.
func (p *ScriptedProvider) Info() provider.Info {
	if p.ProviderInfo.ID != "" {
		return p.ProviderInfo
	}
	return provider.Info{ID: "scripted", APIVersion: "v0"}
}

// Reset rewinds the provider to script index 0 so the same instance
// can serve a fresh sequence (e.g. live run, then replay against the
// same Agent).
func (p *ScriptedProvider) Reset() { p.idx = 0 }

// Stream implements provider.Provider. It does not read the request.
func (p *ScriptedProvider) Stream(_ context.Context, _ *provider.Request) (provider.EventStream, error) {
	if p.OpenErr != nil {
		return nil, p.OpenErr
	}
	if p.idx >= len(p.Scripts) {
		return nil, ErrScriptsExhausted
	}
	s := &scriptedStream{chunks: p.Scripts[p.idx]}
	p.idx++
	return s, nil
}

// NewStream returns a one-shot provider.EventStream that emits chunks
// in order and then returns io.EOF. Useful when a custom provider
// implementation wants to reuse the same chunk-replay semantics as
// ScriptedProvider without subclassing it.
func NewStream(chunks []provider.StreamChunk) provider.EventStream {
	return &scriptedStream{chunks: chunks}
}

// scriptedStream serves one script's chunks and then EOFs.
type scriptedStream struct {
	chunks []provider.StreamChunk
	pos    int
}

func (s *scriptedStream) Next(_ context.Context) (provider.StreamChunk, error) {
	if s.pos >= len(s.chunks) {
		return provider.StreamChunk{}, io.EOF
	}
	c := s.chunks[s.pos]
	s.pos++
	return c, nil
}

func (s *scriptedStream) Close() error { return nil }
