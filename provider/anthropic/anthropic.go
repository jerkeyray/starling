// Package anthropic adapts the Anthropic Messages API to Starling's
// Provider interface with streaming and usage-update normalization.
//
// The adapter uses github.com/anthropics/anthropic-sdk-go for HTTP +
// SSE transport, but all event-payload translation lives here so the
// normalized StreamChunk sequence stays stable across SDK versions.
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

// New constructs a Provider that talks to the Anthropic Messages API.
// WithBaseURL / WithHTTPClient can redirect to a test server or a
// compatible proxy.
func New(opts ...Option) (provider.Provider, error) {
	cfg := config{
		providerID: "anthropic",
		apiVersion: "2023-06-01", // Anthropic's anthropic-version header default
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

// Option configures the Anthropic provider.
type Option func(*config)

type config struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	providerID string
	apiVersion string
}

// WithAPIKey sets the API key used for authentication. If unset, the
// underlying SDK falls back to the ANTHROPIC_API_KEY environment
// variable.
func WithAPIKey(key string) Option { return func(c *config) { c.apiKey = key } }

// WithBaseURL overrides the API base URL.
func WithBaseURL(url string) Option { return func(c *config) { c.baseURL = url } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *config) { cfg.httpClient = c }
}

// WithProviderID overrides the Info().ID string. Useful when a
// compatibility proxy wants to identify itself distinctly in the event
// log. Defaults to "anthropic".
func WithProviderID(id string) Option { return func(c *config) { c.providerID = id } }

// WithAPIVersion overrides the Info().APIVersion string. Defaults to
// "2023-06-01" to match the SDK's default anthropic-version header.
func WithAPIVersion(v string) Option { return func(c *config) { c.apiVersion = v } }

type anthropicProvider struct {
	client *anth.Client
	cfg    config
}

func (p *anthropicProvider) Info() provider.Info {
	return provider.Info{ID: p.cfg.providerID, APIVersion: p.cfg.apiVersion}
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
