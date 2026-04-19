// Command m1_hello is the M1 end-to-end smoke demo. It builds an Agent
// with one tool (current_time), points it at an LLM provider, and
// prints the resulting RunResult plus a one-line dump of the event log.
//
// Usage (OpenAI default):
//
//	OPENAI_API_KEY=sk-... go run ./examples/m1_hello
//
// Anthropic variant:
//
//	PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... go run ./examples/m1_hello
//
// Groq variant (OpenAI-compatible, same binary):
//
//	OPENAI_API_KEY=$GROQ_API_KEY \
//	OPENAI_BASE_URL=https://api.groq.com/openai/v1 \
//	MODEL=llama-3.1-8b-instant \
//	  go run ./examples/m1_hello
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/anthropic"
	"github.com/jerkeyray/starling/provider/openai"
	"github.com/jerkeyray/starling/tool"
)

type clockIn struct{}

type clockOut struct {
	UTC string `json:"utc"`
}

func currentTimeTool() tool.Tool {
	return tool.Typed(
		"current_time",
		"Return the current UTC time in RFC3339.",
		func(_ context.Context, _ clockIn) (clockOut, error) {
			return clockOut{UTC: time.Now().UTC().Format(time.RFC3339)}, nil
		},
	)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// buildProvider picks between OpenAI (default) and Anthropic based on
// the PROVIDER env var. Each branch honours its own MODEL env with a
// sensible default.
func buildProvider() (provider.Provider, string, error) {
	switch os.Getenv("PROVIDER") {
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return nil, "", fmt.Errorf("ANTHROPIC_API_KEY not set")
		}
		model := os.Getenv("MODEL")
		if model == "" {
			model = "claude-sonnet-4-6"
		}
		prov, err := anthropic.New(anthropic.WithAPIKey(apiKey))
		if err != nil {
			return nil, "", fmt.Errorf("anthropic.New: %w", err)
		}
		return prov, model, nil

	default: // "openai" or empty
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return nil, "", fmt.Errorf("OPENAI_API_KEY not set")
		}
		model := os.Getenv("MODEL")
		if model == "" {
			model = "gpt-4o-mini"
		}
		opts := []openai.Option{openai.WithAPIKey(apiKey)}
		if u := os.Getenv("OPENAI_BASE_URL"); u != "" {
			opts = append(opts, openai.WithBaseURL(u))
		}
		prov, err := openai.New(opts...)
		if err != nil {
			return nil, "", fmt.Errorf("openai.New: %w", err)
		}
		return prov, model, nil
	}
}

func run() error {
	prov, model, err := buildProvider()
	if err != nil {
		return err
	}

	log := eventlog.NewInMemory()
	defer log.Close()

	a := &starling.Agent{
		Provider: prov,
		Tools:    []tool.Tool{currentTimeTool()},
		Log:      log,
		Config: starling.Config{
			Model:    model,
			MaxTurns: 4,
		},
	}

	// Honour Ctrl-C so cancellation lands as RunCancelled in the log.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	res, runErr := a.Run(ctx, "What is the current UTC time? Use the current_time tool, then state the time in one short sentence.")

	// Print RunResult + event dump regardless of outcome — diagnostics
	// matter most when the run failed.
	if res != nil {
		printResult(res)
		evs, _ := log.Read(context.Background(), res.RunID)
		printEvents(evs)
		if verr := eventlog.Validate(evs); verr != nil {
			fmt.Println("validate: FAIL:", verr)
		} else {
			fmt.Println("validate: ok")
		}
	}

	if runErr != nil {
		return fmt.Errorf("agent run: %w", runErr)
	}
	return nil
}

func printResult(r *starling.RunResult) {
	fmt.Println("=== RunResult ===")
	fmt.Println("RunID:        ", r.RunID)
	fmt.Println("FinalText:    ", r.FinalText)
	fmt.Println("TerminalKind: ", r.TerminalKind)
	fmt.Println("TurnCount:    ", r.TurnCount)
	fmt.Println("ToolCallCount:", r.ToolCallCount)
	fmt.Println("InputTokens:  ", r.InputTokens)
	fmt.Println("OutputTokens: ", r.OutputTokens)
	fmt.Printf("TotalCostUSD:  %.6f\n", r.TotalCostUSD)
	fmt.Println("Duration:     ", r.Duration)
	fmt.Printf("MerkleRoot:    %x\n", r.MerkleRoot)
}

func printEvents(evs []event.Event) {
	fmt.Println("=== Events ===")
	for _, ev := range evs {
		fmt.Printf("  %3d %s\n", ev.Seq, ev.Kind)
	}
	fmt.Printf("  (%d total)\n", len(evs))
}
