//go:build live

// Live test against the real OpenRouter endpoint. Opt-in: run with
// `go test -tags=live ./provider/openrouter`. Skips if
// OPENROUTER_API_KEY is unset so single-key local runs still pass.

package openrouter_test

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/openrouter"
)

func TestLive_OpenRouter_TextCompletion(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live test")
	}

	p, err := openrouter.New(
		openrouter.WithAPIKey(key),
		openrouter.WithHTTPReferer("https://github.com/jerkeyray/starling"),
		openrouter.WithXTitle("starling-live-test"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := p.Stream(ctx, &provider.Request{
		Model:           "meta-llama/llama-3.2-3b-instruct:free",
		MaxOutputTokens: 32,
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
				t.Errorf("RawResponseHash len = %d", len(c.RawResponseHash))
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
