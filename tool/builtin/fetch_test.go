package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetch_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	tl := Fetch()
	in, _ := json.Marshal(FetchInput{URL: srv.URL})
	out, err := tl.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got FetchOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
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

	tl := Fetch()
	in, _ := json.Marshal(FetchInput{URL: srv.URL})
	out, err := tl.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got FetchOutput
	_ = json.Unmarshal(out, &got)
	if len(got.Body) != fetchMaxBytes {
		t.Fatalf("body len = %d, want %d", len(got.Body), fetchMaxBytes)
	}
}

func TestFetch_NetworkError(t *testing.T) {
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
