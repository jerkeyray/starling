// Package mcp adapts Model Context Protocol tools to Starling tools.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/jerkeyray/starling/tool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultClientName    = "starling"
	defaultClientVersion = "dev"
	defaultMaxOutput     = 1 << 20
)

// Client is a connected MCP client that exposes remote MCP server tools as
// ordinary Starling tools.
type Client struct {
	session *gomcp.ClientSession
	cfg     Config

	mu    sync.RWMutex
	tools []tool.Tool
}

// Config controls MCP tool adaptation.
type Config struct {
	ClientName               string
	ClientVersion            string
	ToolNamePrefix           string
	IncludeTools             []string
	ExcludeTools             []string
	CallTimeout              time.Duration
	MaxOutputBytes           int64
	TextOnly                 bool
	TransientErrorClassifier func(error) bool
}

type Option func(*Config)

// WithClientInfo sets the MCP client implementation name and version.
func WithClientInfo(name, version string) Option {
	return func(c *Config) {
		c.ClientName = name
		c.ClientVersion = version
	}
}

// WithToolNamePrefix prefixes every adapted Starling tool name.
func WithToolNamePrefix(prefix string) Option {
	return func(c *Config) { c.ToolNamePrefix = prefix }
}

// WithIncludeTools limits adaptation to the named MCP tools. Names are matched
// before ToolNamePrefix is applied.
func WithIncludeTools(names ...string) Option {
	return func(c *Config) { c.IncludeTools = append([]string(nil), names...) }
}

// WithExcludeTools excludes the named MCP tools. Names are matched before
// ToolNamePrefix is applied.
func WithExcludeTools(names ...string) Option {
	return func(c *Config) { c.ExcludeTools = append([]string(nil), names...) }
}

// WithCallTimeout wraps each MCP tool call in a timeout. A zero duration leaves
// calls governed by the caller's context.
func WithCallTimeout(timeout time.Duration) Option {
	return func(c *Config) { c.CallTimeout = timeout }
}

// WithMaxOutputBytes caps the JSON-encoded Starling tool result. Values <= 0
// use the package default.
func WithMaxOutputBytes(max int64) Option {
	return func(c *Config) { c.MaxOutputBytes = max }
}

// WithTextOnly returns only text content for unstructured MCP results. Calls
// that return non-text content fail instead of silently dropping data.
func WithTextOnly(textOnly bool) Option {
	return func(c *Config) { c.TextOnly = textOnly }
}

// WithTransientErrorClassifier marks protocol or transport errors as
// retryable. MCP tool-level errors (IsError results) do not pass through this
// hook because they are regular tool outputs from the server's perspective.
func WithTransientErrorClassifier(classifier func(error) bool) Option {
	return func(c *Config) { c.TransientErrorClassifier = classifier }
}

// New connects to an MCP server over transport and discovers its tools.
func New(ctx context.Context, transport gomcp.Transport, opts ...Option) (*Client, error) {
	if transport == nil {
		return nil, errors.New("mcp: nil transport")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	normalizeConfig(&cfg)

	impl := &gomcp.Implementation{Name: cfg.ClientName, Version: cfg.ClientVersion}
	client := gomcp.NewClient(impl, &gomcp.ClientOptions{Capabilities: &gomcp.ClientCapabilities{}})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect: %w", err)
	}
	c := &Client{session: session, cfg: cfg}
	if _, err := c.RefreshTools(ctx); err != nil {
		_ = session.Close()
		return nil, err
	}
	return c, nil
}

// NewCommand starts cmd as an MCP stdio server and discovers its tools.
func NewCommand(ctx context.Context, cmd *exec.Cmd, opts ...Option) (*Client, error) {
	if cmd == nil {
		return nil, errors.New("mcp: nil command")
	}
	return New(ctx, &gomcp.CommandTransport{Command: cmd}, opts...)
}

// NewHTTP connects to a streamable HTTP MCP endpoint and discovers its tools.
func NewHTTP(ctx context.Context, endpoint string, httpClient *http.Client, opts ...Option) (*Client, error) {
	if endpoint == "" {
		return nil, errors.New("mcp: empty endpoint")
	}
	return New(ctx, &gomcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
	}, opts...)
}

// Tools returns the most recently discovered Starling tool wrappers.
func (c *Client) Tools(ctx context.Context) ([]tool.Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]tool.Tool, len(c.tools))
	copy(out, c.tools)
	return out, nil
}

// RefreshTools re-lists remote MCP tools and replaces the cached wrappers.
func (c *Client) RefreshTools(ctx context.Context) ([]tool.Tool, error) {
	var remote []*gomcp.Tool
	for rt, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
		}
		remote = append(remote, rt)
	}

	wrapped, err := c.wrapTools(remote)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.tools = wrapped
	c.mu.Unlock()

	out := make([]tool.Tool, len(wrapped))
	copy(out, wrapped)
	return out, nil
}

// Close closes the underlying MCP session and transport.
func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

func defaultConfig() Config {
	return Config{
		ClientName:     defaultClientName,
		ClientVersion:  defaultClientVersion,
		MaxOutputBytes: defaultMaxOutput,
	}
}

func normalizeConfig(c *Config) {
	if c.ClientName == "" {
		c.ClientName = defaultClientName
	}
	if c.ClientVersion == "" {
		c.ClientVersion = defaultClientVersion
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = defaultMaxOutput
	}
}

func (c *Client) wrapTools(remote []*gomcp.Tool) ([]tool.Tool, error) {
	include := set(c.cfg.IncludeTools)
	exclude := set(c.cfg.ExcludeTools)
	seen := map[string]bool{}
	wrapped := make([]tool.Tool, 0, len(remote))

	for _, rt := range remote {
		if rt == nil {
			continue
		}
		if len(include) > 0 && !include[rt.Name] {
			continue
		}
		if exclude[rt.Name] {
			continue
		}
		name := c.cfg.ToolNamePrefix + rt.Name
		if name == "" {
			return nil, errors.New("mcp: empty tool name")
		}
		if seen[name] {
			return nil, fmt.Errorf("mcp: duplicate tool name %q after prefixing", name)
		}
		seen[name] = true

		schema, err := schemaBytes(rt.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: tool %q schema: %w", rt.Name, err)
		}
		wrapped = append(wrapped, &mcpTool{
			client:      c,
			remoteName:  rt.Name,
			name:        name,
			description: toolDescription(rt),
			schema:      schema,
		})
	}

	sort.SliceStable(wrapped, func(i, j int) bool {
		return wrapped[i].Name() < wrapped[j].Name()
	})
	return wrapped, nil
}

func toolDescription(t *gomcp.Tool) string {
	if t.Description != "" {
		return t.Description
	}
	if t.Title != "" {
		return t.Title
	}
	if t.Annotations != nil && t.Annotations.Title != "" {
		return t.Annotations.Title
	}
	return ""
}

func set(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func schemaBytes(schema any) (json.RawMessage, error) {
	if schema == nil {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	if raw, ok := schema.(json.RawMessage); ok {
		raw = bytes.TrimSpace(raw)
		if !json.Valid(raw) {
			return nil, errors.New("invalid JSON")
		}
		if len(raw) == 0 || raw[0] != '{' {
			return nil, errors.New("schema must be a JSON object")
		}
		out := make(json.RawMessage, len(raw))
		copy(out, raw)
		return out, nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if !json.Valid(b) {
		return nil, errors.New("invalid JSON")
	}
	if len(b) == 0 || b[0] != '{' {
		return nil, errors.New("schema must be a JSON object")
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("schema must be a JSON object")
	}
	out := make(json.RawMessage, len(b))
	copy(out, b)
	return out, nil
}
