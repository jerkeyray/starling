// Command multi_turn shows the recommended pattern for chat-style
// multi-message workflows: one Run per user message.
//
// Each Run is self-contained — its own RunID, its own Merkle root,
// its own replay boundary. The inspector lists them as siblings; the
// /diff page can compare any two; budgets cap each turn-of-the-chat
// independently. If you need the model to "remember" previous
// messages, prepend a summary into the goal text (this example does
// that with the assistant's prior reply).
//
// Why not "one Run for the whole conversation"? Agent.Run always
// emits a terminal event when it returns; you can't append more
// turns to a terminal chain — the Merkle root commits to the leaves
// before it. Resume exists for *crash recovery* on a non-terminal
// chain, not for chat continuation.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/multi_turn
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider/openai"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY not set")
	}

	logStore := eventlog.NewInMemory()
	defer logStore.Close()

	prov, err := openai.New(openai.WithAPIKey(apiKey))
	if err != nil {
		log.Fatalf("openai.New: %v", err)
	}

	agent := &starling.Agent{
		Provider: prov,
		Log:      logStore,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}

	ctx := context.Background()
	messages := []string{
		"Pick a city in Italy.",
		"What's the city's population (rough)?",
		"Name one famous landmark there.",
	}

	var lastReply string
	for i, msg := range messages {
		// Prepend the prior assistant reply as context. This is how
		// you carry conversation state across independent Runs without
		// chaining them into a single Merkle root.
		goal := msg
		if lastReply != "" {
			goal = "Prior reply: " + lastReply + "\n\nNew question: " + msg
		}

		res, err := agent.Run(ctx, goal)
		if err != nil {
			log.Fatalf("Run %d: %v", i+1, err)
		}
		lastReply = strings.TrimSpace(res.FinalText)
		fmt.Printf("--- turn %d ---\n", i+1)
		fmt.Printf("user:      %s\n", msg)
		fmt.Printf("assistant: %s\n", lastReply)
		fmt.Printf("(run=%s, $%.4f, %d in / %d out)\n\n",
			res.RunID, res.TotalCostUSD, res.InputTokens, res.OutputTokens)
	}

	// Each user message is its own Run in the inspector. Open the log
	// in starling-inspect to confirm.
	if lister, ok := logStore.(eventlog.RunLister); ok {
		runs, _ := lister.ListRuns(ctx)
		fmt.Printf("recorded %d independent runs in the log\n", len(runs))
	}
}
