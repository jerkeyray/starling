// Package openrouter is a thin wrapper over provider/openai that
// sets OpenRouter's base URL and optional attribution headers
// (HTTP-Referer, X-Title). Streaming, tool calls, and usage
// accounting are inherited unchanged.
package openrouter

import (
	"net/http"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/openai"
)

// DefaultBaseURL is the public OpenRouter endpoint.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// New constructs a Provider that talks to OpenRouter.
func New(opts ...Option) (provider.Provider, error) {
	cfg := config{
		baseURL:    DefaultBaseURL,
		providerID: "openrouter",
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Layer a RoundTripper that injects attribution headers on top of
	// the caller's client (or a fresh one). Inner transport preserved.
	httpClient := cfg.httpClient
	if cfg.httpReferer != "" || cfg.xTitle != "" {
		base := httpClient
		if base == nil {
			base = &http.Client{}
		}
		httpClient = &http.Client{
			Transport: &headerRoundTripper{
				referer: cfg.httpReferer,
				title:   cfg.xTitle,
				inner:   transportOf(base),
			},
			Timeout:       base.Timeout,
			CheckRedirect: base.CheckRedirect,
			Jar:           base.Jar,
		}
	}

	oaiOpts := []openai.Option{
		openai.WithAPIKey(cfg.apiKey),
		openai.WithBaseURL(cfg.baseURL),
		openai.WithProviderID(cfg.providerID),
	}
	if httpClient != nil {
		oaiOpts = append(oaiOpts, openai.WithHTTPClient(httpClient))
	}
	return openai.New(oaiOpts...)
}

// Option configures the OpenRouter provider.
type Option func(*config)

type config struct {
	apiKey      string
	baseURL     string
	httpReferer string
	xTitle      string
	httpClient  *http.Client
	providerID  string
}

// WithAPIKey sets the OpenRouter API key.
func WithAPIKey(key string) Option { return func(c *config) { c.apiKey = key } }

// WithBaseURL overrides the API base URL. Defaults to DefaultBaseURL.
func WithBaseURL(url string) Option { return func(c *config) { c.baseURL = url } }

// WithHTTPReferer sets the HTTP-Referer header for OpenRouter
// attribution. Optional.
func WithHTTPReferer(url string) Option { return func(c *config) { c.httpReferer = url } }

// WithXTitle sets the X-Title header for OpenRouter attribution.
// Optional.
func WithXTitle(name string) Option { return func(c *config) { c.xTitle = name } }

// WithHTTPClient supplies a custom *http.Client. Attribution headers,
// if set, are layered on top of the caller's transport.
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *config) { cfg.httpClient = c }
}

// WithProviderID overrides Info().ID. Defaults to "openrouter".
func WithProviderID(id string) Option { return func(c *config) { c.providerID = id } }

// headerRoundTripper injects attribution headers. Clones the request
// so the caller's Request is not mutated.
type headerRoundTripper struct {
	referer string
	title   string
	inner   http.RoundTripper
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if h.referer != "" && r.Header.Get("HTTP-Referer") == "" {
		r.Header.Set("HTTP-Referer", h.referer)
	}
	if h.title != "" && r.Header.Get("X-Title") == "" {
		r.Header.Set("X-Title", h.title)
	}
	return h.inner.RoundTrip(r)
}

// transportOf returns c.Transport or http.DefaultTransport when nil.
func transportOf(c *http.Client) http.RoundTripper {
	if c.Transport != nil {
		return c.Transport
	}
	return http.DefaultTransport
}
