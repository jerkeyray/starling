package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
			return nil, fmt.Errorf("mcp tool %q: %w: %w", t.name, err, stool.ErrTransient)
		}
		return nil, fmt.Errorf("mcp tool %q: %w", t.name, err)
	}

	out, err := encodeResult(res, t.client.cfg)
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: %w", t.name, err)
	}
	if res.IsError {
		return nil, &ToolError{Name: t.name, Result: out}
	}
	return out, nil
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
