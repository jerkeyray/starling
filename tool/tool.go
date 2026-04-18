// Package tool defines the Tool interface and the Typed[In, Out] generic
// helper for building typed agent tools.
package tool

import (
	"context"
	"encoding/json"
	"errors"
)

// Tool is the runtime contract every agent tool implements. Implementations
// are expected to be safe for concurrent use by multiple goroutines.
//
// Schema returns a JSON Schema document (parseable as a JSON object)
// describing the shape of Execute's input. The bytes should be stable across
// calls so that RunStarted.ToolSchemas hashes are deterministic.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

// ErrPanicked is returned (wrapped) when a tool function panics during
// Execute. Callers can detect panicked tools via errors.Is(err, ErrPanicked).
var ErrPanicked = errors.New("tool: panicked")
