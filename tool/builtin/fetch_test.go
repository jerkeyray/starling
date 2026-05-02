package builtin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFetch_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	got, err := executeFetch(context.Background(), FetchInput{URL: srv.URL}, srv.Client(), false)
	if err != nil {
		t.Fatalf("executeFetch: %v", err)
	}
	if got.Status != 200 || got.Body != "hello" {
		t.Fatalf("got = %+v", got)
	}
}

func TestFetch_Truncates(t *testing.T) {
	big := strings.Repeat("x", 2<<20) // 2 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	got, err := executeFetch(context.Background(), FetchInput{URL: srv.URL}, srv.Client(), false)
	if err != nil {
		t.Fatalf("executeFetch: %v", err)
	}
	if len(got.Body) != fetchMaxBytes {
		t.Fatalf("body len = %d, want %d", len(got.Body), fetchMaxBytes)
	}
}

func TestFetch_BlocksLoopback(t *testing.T) {
	tl := Fetch()
	in, _ := json.Marshal(FetchInput{URL: "http://127.0.0.1:1/nope"})
	_, err := tl.Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestFetch_MissingURL(t *testing.T) {
	tl := Fetch()
	_, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("expected error for missing URL")
	}
}

func TestFetch_BlocksUnsupportedScheme(t *testing.T) {
	tl := Fetch()
	in, _ := json.Marshal(FetchInput{URL: "file:///etc/passwd"})
	_, err := tl.Execute(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "only http and https") {
		t.Fatalf("err = %v, want unsupported scheme error", err)
	}
}

func TestValidateFetchURL_BlocksPrivateAndLocalIPs(t *testing.T) {
	for _, raw := range []string{
		"http://localhost/",
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/",
		"http://[::1]/",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := validateFetchURL(u); err == nil {
				t.Fatalf("validateFetchURL(%q) = nil, want error", raw)
			}
		})
	}
}

func TestFetchRedirectPolicy_BlocksLocalTarget(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/secret", nil)
	err := safeFetchClient().CheckRedirect(req, []*http.Request{
		httptest.NewRequest(http.MethodGet, "https://example.com/", nil),
	})
	if err == nil {
		t.Fatalf("CheckRedirect = nil, want local target error")
	}
}

func TestSafeFetchDialContext_BlocksLocalResolution(t *testing.T) {
	_, err := safeFetchDialContext(context.Background(), "tcp", net.JoinHostPort("localhost", "80"))
	if err == nil {
		t.Fatalf("safeFetchDialContext = nil, want local host error")
	}
}

func TestSafeFetchClient_DoesNotAssumeDefaultTransportType(t *testing.T) {
	orig := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("default transport should not be used")
		return nil, nil
	})
	t.Cleanup(func() { http.DefaultTransport = orig })

	if safeFetchClient() == nil {
		t.Fatalf("safeFetchClient returned nil")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
