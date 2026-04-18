//go:build live

// Live tests against real backends. Opt-in: run with `go test -tags=live`.
// Each test skips itself if its required API-key env var is unset so local
// runs with only one key still pass.

package openai_test

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/openai"
)

func runHello(t *testing.T, p provider.Provider, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := p.Stream(ctx, &provider.Request{
		Model: model,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "Say hello in one short sentence."},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var sawText, sawEnd bool
	for {
		c, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch c.Kind {
		case provider.ChunkText:
			if c.Text != "" {
				sawText = true
			}
		case provider.ChunkEnd:
			sawEnd = true
			if len(c.RawResponseHash) != 32 {
				t.Fatalf("RawResponseHash len = %d", len(c.RawResponseHash))
			}
		}
	}
	if !sawText {
		t.Fatalf("no ChunkText received")
	}
	if !sawEnd {
		t.Fatalf("no ChunkEnd received")
	}
}

func TestLive_OpenAI(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	p, err := openai.New(openai.WithAPIKey(key))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runHello(t, p, "gpt-4o-mini")
}

func TestLive_Compat_Groq(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		t.Skip("GROQ_API_KEY not set")
	}
	p, err := openai.New(
		openai.WithAPIKey(key),
		openai.WithBaseURL("https://api.groq.com/openai/v1"),
		openai.WithProviderID("groq"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runHello(t, p, "llama-3.1-8b-instant")
}
