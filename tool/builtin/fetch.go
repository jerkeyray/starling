package builtin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jerkeyray/starling/tool"
)

// fetchMaxBytes caps the response body at 1 MiB. Oversize bodies are
// truncated to this limit, not errored — documented behavior.
const fetchMaxBytes = 1 << 20

// fetchTimeout bounds the total round-trip of a single Fetch call.
const fetchTimeout = 15 * time.Second

// FetchInput is the JSON-schema-describing input type for the Fetch tool.
type FetchInput struct {
	URL string `json:"url" jsonschema:"description=Absolute URL to GET."`
}

// FetchOutput is the JSON-schema-describing output type for the Fetch tool.
type FetchOutput struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// Fetch returns a tool that performs an HTTP GET against the given URL and
// returns status code + body (capped at 1 MiB). It only allows http/https
// URLs whose resolved addresses are public internet addresses; localhost,
// private networks, link-local ranges, multicast, and unspecified addresses
// are rejected before dialing and on every redirect.
func Fetch() tool.Tool {
	client := safeFetchClient()
	return tool.Typed[FetchInput, FetchOutput](
		"fetch",
		"Perform an HTTP GET against a public http/https URL and return status + body (body capped at 1 MiB).",
		func(ctx context.Context, in FetchInput) (FetchOutput, error) {
			ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
			defer cancel()
			return executeFetch(ctx, in, client, true)
		},
	)
}

func executeFetch(ctx context.Context, in FetchInput, client *http.Client, validate bool) (FetchOutput, error) {
	if in.URL == "" {
		return FetchOutput{}, fmt.Errorf("fetch: URL is required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	u, err := url.Parse(in.URL)
	if err != nil {
		return FetchOutput{}, fmt.Errorf("fetch: parse URL: %w", err)
	}
	if validate {
		if err := validateFetchURL(u); err != nil {
			return FetchOutput{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
	if err != nil {
		return FetchOutput{}, fmt.Errorf("fetch: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return FetchOutput{}, fmt.Errorf("fetch: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if err != nil {
		return FetchOutput{}, fmt.Errorf("fetch: read body: %w", err)
	}
	return FetchOutput{Status: resp.StatusCode, Body: string(body)}, nil
}

func safeFetchClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           safeFetchDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("fetch: stopped after 10 redirects")
			}
			return validateFetchURL(req.URL)
		},
	}
}

func validateFetchURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("fetch: URL is required")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("fetch: only http and https URLs are allowed")
	}
	if u.Host == "" {
		return fmt.Errorf("fetch: URL host is required")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("fetch: URL host is required")
	}
	if blockedFetchHost(host) {
		return fmt.Errorf("fetch: host %q is private or local", host)
	}
	if ip := net.ParseIP(host); ip != nil && blockedFetchIP(ip) {
		return fmt.Errorf("fetch: host %q resolves to a private or local address", host)
	}
	return nil
}

func safeFetchDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("fetch: split dial address: %w", err)
	}
	ips, err := resolveFetchHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("fetch: host %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if blockedFetchIP(ip) {
			return nil, fmt.Errorf("fetch: host %q resolves to a private or local address", host)
		}
	}
	var lastErr error
	dialer := &net.Dialer{}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("fetch: dial host %q: %w", host, lastErr)
}

func resolveFetchHost(ctx context.Context, host string) ([]net.IP, error) {
	if blockedFetchHost(host) {
		return nil, fmt.Errorf("fetch: host %q is private or local", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolve host %q: %w", host, err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

func blockedFetchHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

func blockedFetchIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}
