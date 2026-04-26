package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jerkeyray/starling/step"
	stool "github.com/jerkeyray/starling/tool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpTool struct {
	client      *Client
	remoteName  string
	name        string
	description string
	schema      json.RawMessage
}

func (t *mcpTool) Name() string        { return t.name }
func (t *mcpTool) Description() string { return t.description }

func (t *mcpTool) Schema() json.RawMessage {
	out := make(json.RawMessage, len(t.schema))
	copy(out, t.schema)
	return out
}

func (t *mcpTool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}

	var args any
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil {
		return nil, fmt.Errorf("mcp tool %q: unmarshal input: %w", t.name, err)
	}
	if args == nil {
		args = map[string]any{}
	}

	// Route through step.SideEffect so the outcome is recorded once at
	// run time and replayed without contacting the MCP server. Outside
	// an agent context (no step.Context attached) we fall back to a
	// direct call.
	live := func() (mcpOutcome, error) { return t.callRemote(ctx, args) }
	if _, ok := step.From(ctx); !ok {
		out, err := live()
		if err != nil {
			return nil, err
		}
		return out.toExecuteResult(t.name)
	}
	out, err := step.SideEffect(ctx, "mcp/"+t.remoteName, live)
	if err != nil {
		return nil, err
	}
	return out.toExecuteResult(t.name)
}

// callRemote performs the live MCP CallTool round-trip. Errors are
// classified and wrapped with tool.ErrTransient when the configured
// classifier reports the error retryable. Tool-level errors (the
// server returned IsError=true) are folded into mcpOutcome rather
// than returned as errors so step.SideEffect records them.
func (t *mcpTool) callRemote(ctx context.Context, args any) (mcpOutcome, error) {
	callCtx := ctx
	cancel := func() {}
	if timeout := t.client.cfg.CallTimeout; timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	res, err := t.client.session.CallTool(callCtx, &gomcp.CallToolParams{
		Name:      t.remoteName,
		Arguments: args,
	})
	if err != nil {
		if t.client.cfg.TransientErrorClassifier != nil && t.client.cfg.TransientErrorClassifier(err) {
			return mcpOutcome{}, fmt.Errorf("mcp tool %q: %w: %w", t.name, err, stool.ErrTransient)
		}
		return mcpOutcome{}, fmt.Errorf("mcp tool %q: %w", t.name, err)
	}

	encoded, err := encodeResult(res, t.client.cfg)
	if err != nil {
		return mcpOutcome{}, fmt.Errorf("mcp tool %q: %w", t.name, err)
	}
	return mcpOutcome{Result: encoded, IsError: res.IsError}, nil
}

// mcpOutcome is the recordable shape of a single MCP tool call.
// Carries both the success and tool-error paths so step.SideEffect can
// replay either without re-contacting the server.
type mcpOutcome struct {
	Result  json.RawMessage `cbor:"result"`
	IsError bool            `cbor:"is_error,omitempty"`
}

func (o mcpOutcome) toExecuteResult(name string) (json.RawMessage, error) {
	if o.IsError {
		return nil, &ToolError{Name: name, Result: o.Result}
	}
	return o.Result, nil
}

// ToolError reports an MCP tool-level error result. Result contains the
// serialized MCP result so callers can inspect the server-provided content.
type ToolError struct {
	Name   string
	Result json.RawMessage
}

func (e *ToolError) Error() string {
	msg := strings.TrimSpace(string(e.Result))
	if msg == "" {
		return fmt.Sprintf("mcp tool %q returned error", e.Name)
	}
	return fmt.Sprintf("mcp tool %q returned error: %s", e.Name, msg)
}

func encodeResult(res *gomcp.CallToolResult, cfg Config) (json.RawMessage, error) {
	if res == nil {
		return nil, errors.New("nil call result")
	}
	if res.StructuredContent != nil {
		return marshalBounded(res.StructuredContent, cfg.MaxOutputBytes)
	}
	if cfg.TextOnly {
		return encodeTextOnly(res, cfg.MaxOutputBytes)
	}
	wire := struct {
		Content []gomcp.Content `json:"content"`
		IsError bool            `json:"is_error,omitempty"`
	}{
		Content: res.Content,
		IsError: res.IsError,
	}
	return marshalBounded(wire, cfg.MaxOutputBytes)
}

func encodeTextOnly(res *gomcp.CallToolResult, max int64) (json.RawMessage, error) {
	text := make([]string, 0, len(res.Content))
	for _, content := range res.Content {
		tc, ok := content.(*gomcp.TextContent)
		if !ok {
			return nil, fmt.Errorf("text-only result contains %T", content)
		}
		text = append(text, tc.Text)
	}
	wire := struct {
		Text    []string `json:"text"`
		IsError bool     `json:"is_error,omitempty"`
	}{
		Text:    text,
		IsError: res.IsError,
	}
	return marshalBounded(wire, max)
}

func marshalBounded(v any, max int64) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("result exceeds max output bytes: %d > %d", len(b), max)
	}
	return json.RawMessage(b), nil
}
