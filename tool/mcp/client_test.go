package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/step"
	"github.com/jerkeyray/starling/tool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClientAdaptsTools(t *testing.T) {
	ctx := context.Background()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "server", Version: "v0.0.1"}, nil)
	server.AddTool(&gomcp.Tool{
		Name:        "echo",
		Description: "echoes a message",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}}}`),
	}, func(_ context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var args struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &gomcp.CallToolResult{
			StructuredContent: map[string]any{"message": args.Message},
		}, nil
	})

	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client, err := New(ctx, clientTransport, WithToolNamePrefix("mcp_"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer client.Close()

	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("len(Tools()) = %d, want 1", len(tools))
	}
	if got, want := tools[0].Name(), "mcp_echo"; got != want {
		t.Fatalf("tool name = %q, want %q", got, want)
	}
	if got, want := tools[0].Description(), "echoes a message"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
	if !json.Valid(tools[0].Schema()) {
		t.Fatalf("schema is not valid JSON: %s", tools[0].Schema())
	}

	out, err := tools[0].Execute(ctx, json.RawMessage(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := string(out), `{"message":"hello"}`; got != want {
		t.Fatalf("Execute() = %s, want %s", got, want)
	}
}

func TestFiltersAndDuplicateNames(t *testing.T) {
	ctx := context.Background()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "server", Version: "v0.0.1"}, nil)
	addNoopTool(server, "a")
	addNoopTool(server, "b")

	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()
	client, err := New(ctx, clientTransport, WithIncludeTools("b"), WithExcludeTools("a"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer client.Close()
	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(tools) != 1 || tools[0].Name() != "b" {
		t.Fatalf("Tools() = %#v, want only b", toolNames(tools))
	}

	_, err = (&Client{}).wrapTools([]*gomcp.Tool{
		{Name: "dup", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "dup", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("wrapTools duplicate error = %v, want duplicate tool name", err)
	}
}

func TestToolDescriptionFallbacks(t *testing.T) {
	tests := []struct {
		name string
		tool *gomcp.Tool
		want string
	}{
		{name: "description", tool: &gomcp.Tool{Description: "description", Title: "title"}, want: "description"},
		{name: "title", tool: &gomcp.Tool{Title: "title"}, want: "title"},
		{name: "annotation title", tool: &gomcp.Tool{Annotations: &gomcp.ToolAnnotations{Title: "annotation"}}, want: "annotation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := toolDescription(test.tool); got != test.want {
				t.Fatalf("toolDescription() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConstructorValidation(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, nil); err == nil || !strings.Contains(err.Error(), "nil transport") {
		t.Fatalf("New(nil) error = %v, want nil transport", err)
	}
	if _, err := NewCommand(ctx, nil); err == nil || !strings.Contains(err.Error(), "nil command") {
		t.Fatalf("NewCommand(nil) error = %v, want nil command", err)
	}
	if _, err := NewHTTP(ctx, "", nil); err == nil || !strings.Contains(err.Error(), "empty endpoint") {
		t.Fatalf("NewHTTP(empty) error = %v, want empty endpoint", err)
	}
}

func TestSchemaValidation(t *testing.T) {
	if _, err := schemaBytes(json.RawMessage(`[]`)); err == nil || !strings.Contains(err.Error(), "schema must be a JSON object") {
		t.Fatalf("schemaBytes(array) error = %v, want object error", err)
	}
	if _, err := schemaBytes(json.RawMessage(`{`)); err == nil {
		t.Fatal("schemaBytes(invalid) error = nil, want error")
	}
	raw, err := schemaBytes(json.RawMessage(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("schemaBytes(raw) error = %v", err)
	}
	if string(raw) != `{"type":"object"}` {
		t.Fatalf("schemaBytes(raw) = %s", raw)
	}
}

func TestMCPErrorResultReturnsToolError(t *testing.T) {
	ctx := context.Background()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "server", Version: "v0.0.1"}, nil)
	server.AddTool(&gomcp.Tool{
		Name:        "fail",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{
			Content: []gomcp.Content{&gomcp.TextContent{Text: "bad input"}},
			IsError: true,
		}, nil
	})

	client := mustInMemoryClient(t, ctx, server)
	defer client.Close()
	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}

	out, err := tools[0].Execute(ctx, nil)
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("Execute() error = %T, want *ToolError", err)
	}
	if out != nil {
		t.Fatalf("Execute() output = %s, want nil on error", out)
	}
	if !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("error %q does not include MCP content", err.Error())
	}
}

func TestTransientClassifier(t *testing.T) {
	ctx := context.Background()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "server", Version: "v0.0.1"}, nil)
	addNoopTool(server, "noop")

	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()
	client, err := New(ctx, clientTransport, WithTransientErrorClassifier(func(error) bool { return true }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer client.Close()

	missing := &mcpTool{
		client:     client,
		remoteName: "missing",
		name:       "missing",
		schema:     json.RawMessage(`{"type":"object"}`),
	}
	_, err = missing.Execute(ctx, nil)
	if !errors.Is(err, tool.ErrTransient) {
		t.Fatalf("Execute() error = %v, want tool.ErrTransient", err)
	}
}

func TestTextOnlyRejectsNonText(t *testing.T) {
	ctx := context.Background()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "server", Version: "v0.0.1"}, nil)
	server.AddTool(&gomcp.Tool{
		Name:        "image",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{
			Content: []gomcp.Content{&gomcp.ImageContent{MIMEType: "image/png", Data: []byte("abc")}},
		}, nil
	})

	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()
	client, err := New(ctx, clientTransport, WithTextOnly(true))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer client.Close()
	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if _, err := tools[0].Execute(ctx, nil); err == nil || !strings.Contains(err.Error(), "text-only") {
		t.Fatalf("Execute() error = %v, want text-only error", err)
	}
}

func TestMaxOutputBytes(t *testing.T) {
	ctx := context.Background()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "server", Version: "v0.0.1"}, nil)
	server.AddTool(&gomcp.Tool{
		Name:        "big",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{StructuredContent: map[string]any{"value": "too long"}}, nil
	})

	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()
	client, err := New(ctx, clientTransport, WithMaxOutputBytes(4))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer client.Close()
	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if _, err := tools[0].Execute(ctx, nil); err == nil || !strings.Contains(err.Error(), "max output") {
		t.Fatalf("Execute() error = %v, want max output error", err)
	}
}

func TestHTTPTransport(t *testing.T) {
	ctx := context.Background()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "server", Version: "v0.0.1"}, nil)
	addNoopTool(server, "noop")

	httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server {
		return server
	}, nil))
	defer httpServer.Close()

	client, err := NewHTTP(ctx, httpServer.URL, httpServer.Client(), WithCallTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewHTTP() error = %v", err)
	}
	defer client.Close()
	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(tools) != 1 || tools[0].Name() != "noop" {
		t.Fatalf("Tools() = %#v, want noop", toolNames(tools))
	}
}

func addNoopTool(server *gomcp.Server, name string) {
	server.AddTool(&gomcp.Tool{
		Name:        name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{StructuredContent: map[string]any{"ok": true}}, nil
	})
}

func mustInMemoryClient(t *testing.T, ctx context.Context, server *gomcp.Server) *Client {
	t.Helper()
	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()
	client, err := New(ctx, clientTransport)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name()
	}
	return out
}

// TestReplayDoesNotContactServer pins the W1-style replay-safety
// guarantee for MCP: a recorded run must replay using the recorded
// SideEffectRecorded value without re-issuing the live MCP CallTool.
func TestReplayDoesNotContactServer(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32

	server := gomcp.NewServer(&gomcp.Implementation{Name: "server", Version: "v0.0.1"}, nil)
	server.AddTool(&gomcp.Tool{
		Name:        "echo",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, _ *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		hits.Add(1)
		return &gomcp.CallToolResult{StructuredContent: map[string]any{"v": 1}}, nil
	})

	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()
	client, err := New(ctx, clientTransport)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	mcpEcho := tools[0]

	// --- live: record the SideEffect via a step.Context ----------------
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	const runID = "mcp-replay-1"
	if err := log.Append(ctx, runID, seedRunStarted(t, runID)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	live := step.MustNewContext(step.Config{
		Log:                log,
		RunID:              runID,
		ResumeFromSeq:      1,
		ResumeFromPrevHash: prevHashOf(t, runID, log),
	})
	liveCtx := step.WithContext(ctx, live)
	out, err := mcpEcho.Execute(liveCtx, nil)
	if err != nil {
		t.Fatalf("live Execute: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits after live = %d, want 1", hits.Load())
	}

	// --- replay: feed the recorded events back; expect zero new hits --
	recorded, err := log.Read(ctx, runID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	replaySink := eventlog.NewInMemory()
	t.Cleanup(func() { _ = replaySink.Close() })
	if err := replaySink.Append(ctx, runID, recorded[0]); err != nil {
		t.Fatalf("seed replay sink: %v", err)
	}
	enc, err := event.Marshal(recorded[0])
	if err != nil {
		t.Fatalf("Marshal seed: %v", err)
	}
	replayCtx := step.MustNewContext(step.Config{
		Log:                replaySink,
		RunID:              runID,
		Mode:               step.ModeReplay,
		Recorded:           recorded,
		ResumeFromSeq:      1,
		ResumeFromPrevHash: event.Hash(enc),
	})
	replayOut, err := mcpEcho.Execute(step.WithContext(ctx, replayCtx), nil)
	if err != nil {
		t.Fatalf("replay Execute: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits after replay = %d, want 1 (replay must not contact the server)", hits.Load())
	}
	if string(replayOut) != string(out) {
		t.Fatalf("replay output = %s, want %s", replayOut, out)
	}
}

func seedRunStarted(t *testing.T, runID string) event.Event {
	t.Helper()
	payload, err := event.EncodePayload(event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          "test",
		ProviderID:    "test",
		ModelID:       "test",
	})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	return event.Event{
		RunID:     runID,
		Seq:       1,
		Timestamp: 1,
		Kind:      event.KindRunStarted,
		Payload:   payload,
	}
}

func prevHashOf(t *testing.T, runID string, log eventlog.EventLog) []byte {
	t.Helper()
	evs, err := log.Read(context.Background(), runID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(evs) == 0 {
		return nil
	}
	last := evs[len(evs)-1]
	enc, err := event.Marshal(last)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return event.Hash(enc)
}
