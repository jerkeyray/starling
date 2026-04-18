package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type greetIn struct {
	Name string `json:"name"`
}
type greetOut struct {
	Greeting string `json:"greeting"`
}

func TestTyped_HappyPath(t *testing.T) {
	tl := Typed[greetIn, greetOut](
		"greet",
		"Greet a person.",
		func(_ context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "hello, " + in.Name}, nil
		},
	)
	if tl.Name() != "greet" {
		t.Fatalf("Name = %q", tl.Name())
	}
	if tl.Description() != "Greet a person." {
		t.Fatalf("Description = %q", tl.Description())
	}
	if len(tl.Schema()) == 0 {
		t.Fatalf("Schema is empty")
	}

	out, err := tl.Execute(context.Background(), json.RawMessage(`{"name":"alice"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got greetOut
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Greeting != "hello, alice" {
		t.Fatalf("got = %q", got.Greeting)
	}
}

func TestTyped_InvalidJSON(t *testing.T) {
	tl := Typed[greetIn, greetOut]("greet", "",
		func(_ context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: in.Name}, nil
		},
	)
	_, err := tl.Execute(context.Background(), json.RawMessage(`{not json`))
	if err == nil {
		t.Fatalf("expected error for invalid JSON")
	}
}

func TestTyped_FunctionError(t *testing.T) {
	sentinel := errors.New("boom")
	tl := Typed[greetIn, greetOut]("greet", "",
		func(_ context.Context, _ greetIn) (greetOut, error) {
			return greetOut{}, sentinel
		},
	)
	_, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestTyped_PanicRecovery(t *testing.T) {
	tl := Typed[greetIn, greetOut]("boom", "",
		func(_ context.Context, _ greetIn) (greetOut, error) {
			panic("kaboom")
		},
	)
	_, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, ErrPanicked) {
		t.Fatalf("err = %v, want wrapping ErrPanicked", err)
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("err = %v, want panic value in message", err)
	}
}

func TestTyped_NonStructInPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for non-struct In")
		}
	}()
	_ = Typed[string, greetOut]("x", "",
		func(_ context.Context, _ string) (greetOut, error) { return greetOut{}, nil },
	)
}

func TestTyped_SchemaReturnsCopy(t *testing.T) {
	tl := Typed[greetIn, greetOut]("greet", "",
		func(_ context.Context, _ greetIn) (greetOut, error) { return greetOut{}, nil },
	)
	a := tl.Schema()
	if len(a) == 0 {
		t.Fatalf("empty schema")
	}
	a[0] = 'X'
	b := tl.Schema()
	if b[0] == 'X' {
		t.Fatalf("Schema() returned aliased slice — caller mutation leaked")
	}
}

func TestTyped_EmptyInputAcceptsZeroValue(t *testing.T) {
	tl := Typed[greetIn, greetOut]("greet", "",
		func(_ context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "hi " + in.Name}, nil
		},
	)
	out, err := tl.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got greetOut
	_ = json.Unmarshal(out, &got)
	if got.Greeting != "hi " {
		t.Fatalf("got = %q", got.Greeting)
	}
}
