// Package gemini adapts the Google Gemini API to Starling's Provider
// interface with streaming and usage-update normalization.
//
// The adapter wraps google.golang.org/genai for HTTP + SSE transport,
// but all payload translation lives here so the normalized StreamChunk
// sequence stays stable across SDK versions. Only the Gemini API
// backend is supported today; Vertex AI (OAuth / service-account auth)
// is a deliberate non-goal for this milestone.
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
// WithBaseURL / WithHTTPClient can redirect to a test server or a
// compatible proxy.
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

// WithAPIKey sets the API key used for authentication. If unset, the
// underlying SDK falls back to the GEMINI_API_KEY or GOOGLE_API_KEY
// environment variable.
func WithAPIKey(key string) Option { return func(c *config) { c.apiKey = key } }

// WithBaseURL overrides the API base URL. Useful for tests and for
// routing through compatible proxies.
func WithBaseURL(url string) Option { return func(c *config) { c.baseURL = url } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *config) { cfg.httpClient = c }
}

// WithProviderID overrides the Info().ID string. Useful when a
// compatibility proxy wants to identify itself distinctly in the event
// log. Defaults to "gemini".
func WithProviderID(id string) Option { return func(c *config) { c.providerID = id } }

// WithAPIVersion overrides the Info().APIVersion string and the API
// version used on the wire. Defaults to "v1beta" — the current stable
// Gemini REST endpoint prefix.
func WithAPIVersion(v string) Option { return func(c *config) { c.apiVersion = v } }

type geminiProvider struct {
	client *genai.Client
	cfg    config
}

func (p *geminiProvider) Info() provider.Info {
	return provider.Info{ID: p.cfg.providerID, APIVersion: p.cfg.apiVersion}
}

func (p *geminiProvider) Stream(ctx context.Context, req *provider.Request) (provider.EventStream, error) {
	if req == nil {
		return nil, errors.New("gemini: nil Request")
	}

	contents, genCfg, err := buildParams(req)
	if err != nil {
		return nil, err
	}

	// Kick off the streaming call. The SDK returns an iter.Seq2 that
	// yields one GenerateContentResponse per SSE frame; we pull from
	// it one chunk at a time via iter.Pull2 inside newGeminiStream.
	iter := p.client.Models.GenerateContentStream(ctx, req.Model, contents, genCfg)
	return newGeminiStream(iter), nil
}
