package provider

import (
	"errors"
	"fmt"
	"net"
	"net/url"
)

// Sentinel error categories returned by adapter Stream methods. Adapters
// wrap their underlying HTTP / SDK error with one of these so callers
// can write retry/back-off policy via errors.Is without parsing vendor-
// specific messages.
//
// Categories are non-overlapping: an error may wrap at most one of
// these. The mapping is by HTTP status (or vendor-equivalent):
//
//   - 400-class auth/permission       → ErrAuth
//   - 429 / quota / rate              → ErrRateLimit
//   - 500-class server / upstream     → ErrServer
//   - transport / DNS / connection    → ErrNetwork
//
// 4xx errors that are neither auth nor rate-limit (e.g. invalid
// request, model not found) are returned unwrapped: they reflect a
// caller bug, not a transient or retryable condition.
var (
	// ErrRateLimit indicates the upstream has rejected the call due
	// to request-rate or quota limits. Typically retryable after a
	// back-off; vendors may include Retry-After hints in the wrapped
	// error's underlying value.
	ErrRateLimit = errors.New("provider: rate limited")

	// ErrAuth indicates the request was rejected for credential or
	// permission reasons (missing/invalid API key, blocked org,
	// expired token). Not retryable; surfaces a configuration bug.
	ErrAuth = errors.New("provider: authentication failed")

	// ErrServer indicates an upstream server-side failure (5xx).
	// Usually retryable after a short back-off.
	ErrServer = errors.New("provider: server error")

	// ErrNetwork indicates a transport-level failure: DNS, TCP/TLS
	// connection, broken stream, vendor SDK timeouts. Usually
	// retryable.
	ErrNetwork = errors.New("provider: network error")
)

// WrapHTTPStatus annotates err with one of the provider sentinels
// based on the HTTP status code an SDK reported. Adapters call this
// from their Stream() entry point so callers can write retry policy
// via errors.Is(err, provider.ErrRateLimit) without parsing message
// strings.
//
// Status 0 means "no HTTP layer" — typically a transport failure that
// never reached the server. In that case the error is classified via
// ClassifyTransport (DNS / connection / TLS).
//
// Statuses outside the wrapped categories pass through unmodified.
func WrapHTTPStatus(err error, status int) error {
	if err == nil {
		return nil
	}
	switch {
	case status == 0:
		return ClassifyTransport(err)
	case status == 401, status == 403:
		return fmt.Errorf("%w: %w", ErrAuth, err)
	case status == 429:
		return fmt.Errorf("%w: %w", ErrRateLimit, err)
	case status >= 500:
		return fmt.Errorf("%w: %w", ErrServer, err)
	}
	return err
}

// ClassifyTransport wraps err with ErrNetwork when it represents a
// transport-level failure (DNS, dial, TLS, broken connection, SDK
// timeout). Other errors pass through unmodified.
func ClassifyTransport(err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	return err
}
