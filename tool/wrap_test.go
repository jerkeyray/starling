package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jerkeyray/starling/tool"
)

type pingIn struct {
	Msg string `json:"msg"`
}
type pingOut struct {
	Got string `json:"got"`
}

func newPing() tool.Tool {
	return tool.Typed("ping", "echo msg", func(_ context.Context, in pingIn) (pingOut, error) {
		return pingOut{Got: in.Msg}, nil
	})
}

func TestWrap_NoMiddlewareIsIdentity(t *testing.T) {
	base := newPing()
	wrapped := tool.Wrap(base)
	if wrapped != base {
		t.Fatalf("Wrap with no middleware should return the same Tool")
	}
}

func TestWrap_PreservesSurface(t *testing.T) {
	base := newPing()
	wrapped := tool.Wrap(base, func(inner func(context.Context, json.RawMessage) (json.RawMessage, error)) func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return inner
	})
	if wrapped.Name() != base.Name() {
		t.Fatalf("Name: got %q, want %q", wrapped.Name(), base.Name())
	}
	if wrapped.Description() != base.Description() {
		t.Fatalf("Description: got %q, want %q", wrapped.Description(), base.Description())
	}
	if string(wrapped.Schema()) != string(base.Schema()) {
		t.Fatalf("Schema: got %s, want %s", wrapped.Schema(), base.Schema())
	}
}

func TestWrap_OrderingIsOuterFirst(t *testing.T) {
	var trace []string
	mk := func(label string) tool.Middleware {
		return func(inner func(context.Context, json.RawMessage) (json.RawMessage, error)) func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
				trace = append(trace, "before "+label)
				out, err := inner(ctx, in)
				trace = append(trace, "after "+label)
				return out, err
			}
		}
	}
	wrapped := tool.Wrap(newPing(), mk("A"), mk("B"))
	if _, err := wrapped.Execute(context.Background(), json.RawMessage(`{"msg":"x"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.Join(trace, ",")
	want := "before A,before B,after B,after A"
	if got != want {
		t.Fatalf("trace = %q, want %q", got, want)
	}
}

func TestWrap_ShortCircuitSkipsInner(t *testing.T) {
	denied := errors.New("denied")
	auth := func(_ func(context.Context, json.RawMessage) (json.RawMessage, error)) func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return nil, denied
		}
	}
	called := 0
	count := func(inner func(context.Context, json.RawMessage) (json.RawMessage, error)) func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			called++
			return inner(ctx, in)
		}
	}
	wrapped := tool.Wrap(newPing(), auth, count)
	_, err := wrapped.Execute(context.Background(), json.RawMessage(`{"msg":"x"}`))
	if !errors.Is(err, denied) {
		t.Fatalf("err = %v, want denied", err)
	}
	if called != 0 {
		t.Fatalf("inner middleware ran %d times after short-circuit", called)
	}
}
