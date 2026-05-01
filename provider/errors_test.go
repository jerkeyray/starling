package provider_test

import (
	"errors"
	"net"
	"net/url"
	"testing"

	"github.com/jerkeyray/starling/provider"
)

func TestWrapHTTPStatus_MapsCategories(t *testing.T) {
	base := errors.New("upstream said no")
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"401 → Auth", 401, provider.ErrAuth},
		{"403 → Auth", 403, provider.ErrAuth},
		{"429 → RateLimit", 429, provider.ErrRateLimit},
		{"500 → Server", 500, provider.ErrServer},
		{"503 → Server", 503, provider.ErrServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := provider.WrapHTTPStatus(base, tc.status)
			if !errors.Is(got, tc.want) {
				t.Fatalf("status %d: errors.Is(_, %v) = false; got %v", tc.status, tc.want, got)
			}
			if !errors.Is(got, base) {
				t.Fatalf("status %d: lost the underlying error", tc.status)
			}
		})
	}
}

func TestWrapHTTPStatus_PassesThroughUnknown(t *testing.T) {
	base := errors.New("400 bad request")
	got := provider.WrapHTTPStatus(base, 400)
	if got != base {
		t.Fatalf("WrapHTTPStatus(_, 400) = %v, want passthrough %v", got, base)
	}
	for _, sentinel := range []error{provider.ErrAuth, provider.ErrRateLimit, provider.ErrServer, provider.ErrNetwork} {
		if errors.Is(got, sentinel) {
			t.Fatalf("400 leaked into sentinel %v", sentinel)
		}
	}
}

func TestWrapHTTPStatus_NilErrIsNil(t *testing.T) {
	if got := provider.WrapHTTPStatus(nil, 500); got != nil {
		t.Fatalf("nil err became %v", got)
	}
}

func TestClassifyTransport_NetError(t *testing.T) {
	// net.OpError satisfies net.Error; ClassifyTransport must wrap it.
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	got := provider.ClassifyTransport(netErr)
	if !errors.Is(got, provider.ErrNetwork) {
		t.Fatalf("net.OpError did not map to ErrNetwork: got %v", got)
	}
}

func TestClassifyTransport_URLError(t *testing.T) {
	urlErr := &url.Error{Op: "Get", URL: "https://x", Err: errors.New("eof")}
	got := provider.ClassifyTransport(urlErr)
	if !errors.Is(got, provider.ErrNetwork) {
		t.Fatalf("*url.Error did not map to ErrNetwork: got %v", got)
	}
}

func TestClassifyTransport_PassesThroughOtherErrors(t *testing.T) {
	plain := errors.New("not a transport error")
	if got := provider.ClassifyTransport(plain); got != plain {
		t.Fatalf("ClassifyTransport unexpectedly wrapped %v as %v", plain, got)
	}
}

func TestWrapHTTPStatus_ZeroStatusFallsThroughClassifyTransport(t *testing.T) {
	urlErr := &url.Error{Op: "Get", URL: "https://x", Err: errors.New("eof")}
	got := provider.WrapHTTPStatus(urlErr, 0)
	if !errors.Is(got, provider.ErrNetwork) {
		t.Fatalf("status=0 should fall through to ClassifyTransport: got %v", got)
	}
}
