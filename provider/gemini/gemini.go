// Package gemini adapts the Google Gemini API to Starling's Provider
// interface. Wraps google.golang.org/genai for transport; payload
// translation lives here. Vertex AI backend is not supported.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/genai"

	"github.com/jerkeyray/starling/provider"
)

// New constructs a Provider that talks to the Gemini API.
func New(opts ...Option) (provider.Provider, error) {
	cfg := config{
		providerID: "gemini",
		apiVersion: "v1beta",
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	clientCfg := &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI,
		APIKey:  cfg.apiKey,
	}
	if cfg.baseURL != "" {
		clientCfg.HTTPOptions.BaseURL = cfg.baseURL
	}
	if cfg.apiVersion != "" {
		clientCfg.HTTPOptions.APIVersion = cfg.apiVersion
	}
	if cfg.httpClient != nil {
		clientCfg.HTTPClient = cfg.httpClient
	}

	client, err := genai.NewClient(context.Background(), clientCfg)
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	return &geminiProvider{client: client, cfg: cfg}, nil
}

// Option configures the Gemini provider.
type Option func(*config)

type config struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	providerID string
	apiVersion string
}

// WithAPIKey sets the API key. If unset, the SDK falls back to
// GEMINI_API_KEY / GOOGLE_API_KEY.
func WithAPIKey(key string) Option { return func(c *config) { c.apiKey = key } }

// WithBaseURL overrides the API base URL.
func WithBaseURL(url string) Option { return func(c *config) { c.baseURL = url } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *config) { cfg.httpClient = c }
}

// WithProviderID overrides Info().ID. Defaults to "gemini".
func WithProviderID(id string) Option { return func(c *config) { c.providerID = id } }

// WithAPIVersion overrides the API version. Defaults to "v1beta".
func WithAPIVersion(v string) Option { return func(c *config) { c.apiVersion = v } }

type geminiProvider struct {
	client *genai.Client
	cfg    config
}

func (p *geminiProvider) Info() provider.Info {
	return provider.Info{ID: p.cfg.providerID, APIVersion: p.cfg.apiVersion}
}

func (p *geminiProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Tools:         true,
		StopSequences: true,
		RequestID:     true,
	}
}

func (p *geminiProvider) Stream(ctx context.Context, req *provider.Request) (provider.EventStream, error) {
	if req == nil {
		return nil, errors.New("gemini: nil Request")
	}

	contents, genCfg, err := buildParams(req)
	if err != nil {
		return nil, err
	}
	iter := p.client.Models.GenerateContentStream(ctx, req.Model, contents, genCfg)
	return newGeminiStream(iter), nil
}
