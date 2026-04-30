//go:build live

package bedrock

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/jerkeyray/starling/provider"
)

// TestLive_TextOnly verifies the adapter against a real Bedrock model.
//
// Run with:
//
//	BEDROCK_MODEL_ID=... AWS_REGION=us-east-1 go test -tags=live ./provider/bedrock
func TestLive_TextOnly(t *testing.T) {
	model := os.Getenv("BEDROCK_MODEL_ID")
	if model == "" {
		t.Skip("BEDROCK_MODEL_ID not set")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		t.Skip("AWS_REGION/AWS_DEFAULT_REGION not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatalf("LoadDefaultConfig: %v", err)
	}
	p, err := New(WithAWSConfig(awsCfg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := p.Stream(ctx, &provider.Request{
		Model: model,
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: "Say hello in one short sentence.",
		}},
		MaxOutputTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var usageSeen, endSeen bool
	for {
		chunk, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch chunk.Kind {
		case provider.ChunkText:
			text.WriteString(chunk.Text)
		case provider.ChunkUsage:
			usageSeen = chunk.Usage != nil
		case provider.ChunkEnd:
			endSeen = true
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Fatal("no text returned")
	}
	if !usageSeen {
		t.Fatal("no usage chunk returned")
	}
	if !endSeen {
		t.Fatal("no end chunk returned")
	}
}
