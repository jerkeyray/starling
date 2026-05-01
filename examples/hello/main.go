// Command hello is a minimal Starling agent: ~50 lines from imports
// to printed output. Build the provider from OPENAI_API_KEY, run one
// goal, print the final text. No replay, no inspect, no flags.
//
//	OPENAI_API_KEY=sk-... go run ./examples/hello
//
// For a comprehensive example covering inspect, replay, MCP tools,
// budgets, and metrics, see examples/m1_hello and
// examples/incident_triage.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider/openai"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY not set")
	}

	prov, err := openai.New(openai.WithAPIKey(apiKey))
	if err != nil {
		log.Fatalf("openai.New: %v", err)
	}

	logStore := eventlog.NewInMemory()
	defer logStore.Close()

	agent := &starling.Agent{
		Provider: prov,
		Log:      logStore,
		Config: starling.Config{
			Model:    "gpt-4o-mini",
			MaxTurns: 4,
		},
	}

	res, err := agent.Run(context.Background(), "Give me a three-bullet summary of what an event-sourced agent runtime is.")
	if err != nil {
		log.Fatalf("Run: %v", err)
	}
	fmt.Println(res.FinalText)
}
