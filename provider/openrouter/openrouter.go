// Package openrouter adapts the OpenRouter API to Starling's Provider
// interface. OpenRouter speaks the OpenAI Chat Completions wire format,
// so this package is a thin wrapper over provider/openai that sets the
// base URL, the default provider ID, and optional attribution headers
// (HTTP-Referer, X-Title) that OpenRouter uses to credit traffic on its
// leaderboard.
//
// Usage:
//
//	p, err := openrouter.New(
//	    openrouter.WithAPIKey(os.Getenv("OPENROUTER_API_KEY")),
//	    openrouter.WithHTTPReferer("https://myapp.example"),
//	    openrouter.WithXTitle("my-app"),
//	)
//
// The constructor returns a standard provider.Provider — streaming,
// tool calls, usage accounting, and the RawResponseHash chain are all
// inherited from the OpenAI adapter unchanged.
package openrouter

import (
	"net/http"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/openai"
)

// DefaultBaseURL is the public OpenRouter endpoint. Override with
// WithBaseURL for tests or when routing through a compatible proxy.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// New constructs a Provider that talks to OpenRouter. It forwards to
// openai.New with OpenRouter-appropriate defaults.
func New(opts ...Option) (provider.Provider, error) {
	cfg := config{
		baseURL:    DefaultBaseURL,
		providerID: "openrouter",
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// If attribution headers are requested, layer a RoundTripper that
	// injects them over whatever *http.Client the caller supplied (or
	// a fresh default client otherwise). The inner transport is
	// preserved so custom transports / proxies still work.
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

// WithAPIKey sets the OpenRouter API key used for authentication.
func WithAPIKey(key string) Option { return func(c *config) { c.apiKey = key } }

// WithBaseURL overrides the API base URL. Defaults to DefaultBaseURL;
// override for tests (httptest server) or for proxies that front
// OpenRouter.
func WithBaseURL(url string) Option { return func(c *config) { c.baseURL = url } }

// WithHTTPReferer sets the HTTP-Referer header OpenRouter uses for
// traffic attribution on its public leaderboard. Optional.
func WithHTTPReferer(url string) Option { return func(c *config) { c.httpReferer = url } }

// WithXTitle sets the X-Title header OpenRouter uses for traffic
// attribution on its public leaderboard. Optional.
func WithXTitle(name string) Option { return func(c *config) { c.xTitle = name } }

// WithHTTPClient supplies a custom *http.Client. If attribution
// headers are also configured, they are layered on top of the caller's
// transport.
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *config) { cfg.httpClient = c }
}

// WithProviderID overrides the Info().ID string. Defaults to
// "openrouter".
func WithProviderID(id string) Option { return func(c *config) { c.providerID = id } }

// headerRoundTripper injects OpenRouter attribution headers on every
// outgoing request. Request is cloned so callers' Request objects are
// not mutated.
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
