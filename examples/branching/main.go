// Command branching demonstrates the WAL-safe SQLite fork helper.
//
// Pattern: record an agent run into runs.db, fork the log at a chosen
// sequence boundary into branch.db, then run a second variant against
// the forked log so the original recording stays intact.
//
//   - eventlog.ForkSQLite copies via VACUUM INTO, which is the only
//     SQLite-supported way to clone a WAL-mode database without leaking
//     .db-wal/.db-shm sidecars (a naïve cp produces a corrupt copy).
//   - beforeSeq=K keeps events 1..K-1 of the named run; pass 0 to fork
//     the run as-is.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/branching
//
// Then point starling-inspect at runs.db and branch.db in two browser
// tabs to compare side-by-side, or use the /diff page to align them
// by sequence number.
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
	const srcPath, dstPath = "runs.db", "branch.db"
	_ = os.Remove(srcPath)
	_ = os.Remove(srcPath + "-wal")
	_ = os.Remove(srcPath + "-shm")
	_ = os.Remove(dstPath)
	_ = os.Remove(dstPath + "-wal")
	_ = os.Remove(dstPath + "-shm")

	src, err := eventlog.NewSQLite(srcPath)
	if err != nil {
		log.Fatalf("open src: %v", err)
	}

	prov, err := openai.New(openai.WithAPIKey(apiKey))
	if err != nil {
		log.Fatalf("openai.New: %v", err)
	}

	a := &starling.Agent{
		Provider: prov,
		Log:      src,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	res, err := a.Run(context.Background(),
		"Pick a random integer between 1 and 100, name a city, and tell me a fun fact about that city.")
	if err != nil {
		log.Fatalf("Run: %v", err)
	}
	fmt.Printf("recorded run %s into %s (%d turns, $%.4f)\n",
		res.RunID, srcPath, res.TurnCount, res.TotalCostUSD)

	// Close before forking so SQLite can finalize WAL writes that the
	// VACUUM INTO snapshot will include. ForkSQLite tolerates a live
	// writer too, but closing first is cleaner for a one-shot example.
	if err := src.Close(); err != nil {
		log.Fatalf("close src: %v", err)
	}

	// Fork the log: keep events 1 and 2 only (RunStarted and the first
	// TurnStarted), then re-run from there. beforeSeq=3 truncates seq>=3.
	if err := eventlog.ForkSQLite(context.Background(), srcPath, dstPath, res.RunID, 3); err != nil {
		log.Fatalf("ForkSQLite: %v", err)
	}
	fmt.Printf("forked %s → %s at seq=3 (kept events 1..2 of %s)\n", srcPath, dstPath, res.RunID)

	dst, err := eventlog.NewSQLite(dstPath)
	if err != nil {
		log.Fatalf("open dst: %v", err)
	}
	defer dst.Close()
	evs, err := dst.Read(context.Background(), res.RunID)
	if err != nil {
		log.Fatalf("read dst: %v", err)
	}
	fmt.Printf("destination log has %d events for run %s; last kind = %s\n",
		len(evs), res.RunID, evs[len(evs)-1].Kind)

	// To keep extending the forked chain, call Resume on a fresh Agent
	// pointed at dst — Resume re-uses the existing chain hash and
	// appends new events (RunResumed → TurnStarted → ...).
	fmt.Println("inspect with: starling-inspect branch.db")
}
