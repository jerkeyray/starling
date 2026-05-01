// Package anthropic adapts the Anthropic Messages API to Starling's
// Provider interface. Transport uses anthropic-sdk-go; event-payload
// translation lives here so StreamChunk output stays stable across
// SDK versions.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/jerkeyray/starling/provider"
)

// DefaultMaxOutputTokens is substituted when a caller leaves
// Request.MaxOutputTokens at zero. Anthropic requires a positive
// max_tokens on every request; 4096 matches the Messages-API default
// example and is well below any in-tree model's hard cap.
const DefaultMaxOutputTokens = 4096

// New constructs a Provider for the Anthropic Messages API.
func New(opts ...Option) (provider.Provider, error) {
	cfg := config{
		providerID: "anthropic",
		apiVersion: "2023-06-01", // anthropic-version header
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	var clientOpts []option.RequestOption
	if cfg.apiKey != "" {
		clientOpts = append(clientOpts, option.WithAPIKey(cfg.apiKey))
	}
	if cfg.baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(cfg.baseURL))
	}
	if cfg.httpClient != nil {
		clientOpts = append(clientOpts, option.WithHTTPClient(cfg.httpClient))
	}

	client := anth.NewClient(clientOpts...)
	return &anthropicProvider{client: &client, cfg: cfg}, nil
}

type Option func(*config)

type config struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	providerID string
	apiVersion string
}

// WithAPIKey sets the API key. Falls back to ANTHROPIC_API_KEY env if unset.
func WithAPIKey(key string) Option { return func(c *config) { c.apiKey = key } }

func WithBaseURL(url string) Option { return func(c *config) { c.baseURL = url } }

func WithHTTPClient(c *http.Client) Option {
	return func(cfg *config) { cfg.httpClient = c }
}

// WithProviderID overrides the Info().ID, useful when a compatibility
// proxy wants a distinct label in the event log.
func WithProviderID(id string) Option { return func(c *config) { c.providerID = id } }

func WithAPIVersion(v string) Option { return func(c *config) { c.apiVersion = v } }

type anthropicProvider struct {
	client *anth.Client
	cfg    config
}

func (p *anthropicProvider) Info() provider.Info {
	return provider.Info{ID: p.cfg.providerID, APIVersion: p.cfg.apiVersion}
}

func (p *anthropicProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Tools:         true,
		ToolChoice:    true,
		Reasoning:     true,
		StopSequences: true,
		CacheControl:  true,
		RequestID:     true,
	}
}

func (p *anthropicProvider) Stream(ctx context.Context, req *provider.Request) (provider.EventStream, error) {
	if req == nil {
		return nil, errors.New("anthropic: nil Request")
	}

	params, err := buildParams(req)
	if err != nil {
		return nil, err
	}

	extraOpts, err := paramsFromCBOR(req.Params)
	if err != nil {
		return nil, fmt.Errorf("anthropic: decode Request.Params: %w", err)
	}

	httpResp := new(*http.Response)
	reqOpts := append(extraOpts, option.WithResponseInto(httpResp))

	sdk := p.client.Messages.NewStreaming(ctx, params, reqOpts...)
	return newAnthropicStream(sdk, httpResp), nil
}
