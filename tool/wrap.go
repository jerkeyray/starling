package tool

import (
	"context"
	"encoding/json"
)

// Middleware wraps a tool's Execute. It is given the inner Execute
// function and must return a function with the same signature; the
// returned function is what the runtime ultimately calls.
//
// Use cases: logging, timing, request authentication, span injection,
// input validation that runs before the tool, output redaction. A
// middleware that wants to short-circuit can return without calling
// inner; one that wants to observe can wrap the call site with
// before/after logic.
//
// Middleware composes via Wrap: the first Middleware passed wraps the
// inner-most call (closest to the original Execute), the last one
// passed runs first. This matches net/http.Handler chaining.
type Middleware func(inner func(context.Context, json.RawMessage) (json.RawMessage, error)) func(context.Context, json.RawMessage) (json.RawMessage, error)

// Wrap returns a new Tool whose Execute is t.Execute with the given
// middleware composed around it. Name, Description, and Schema pass
// through unchanged so the model sees the same contract; only the
// runtime call path is layered.
//
// Example:
//
//	timed := tool.Wrap(myTool, withTiming, withAuth)
//
// withAuth runs first; if it short-circuits, withTiming and the
// inner Execute are skipped.
func Wrap(t Tool, mw ...Middleware) Tool {
	if len(mw) == 0 {
		return t
	}
	exec := t.Execute
	for i := len(mw) - 1; i >= 0; i-- {
		exec = mw[i](exec)
	}
	return &wrappedTool{inner: t, exec: exec}
}

type wrappedTool struct {
	inner Tool
	exec  func(context.Context, json.RawMessage) (json.RawMessage, error)
}

func (w *wrappedTool) Name() string            { return w.inner.Name() }
func (w *wrappedTool) Description() string     { return w.inner.Description() }
func (w *wrappedTool) Schema() json.RawMessage { return w.inner.Schema() }
func (w *wrappedTool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return w.exec(ctx, input)
}
