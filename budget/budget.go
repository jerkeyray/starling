// Package budget defines cost and token budgets and the enforcement logic
// that cancels in-flight LLM streams when a budget trips.
package budget

import "time"

// Budget caps enforced across a run. Zero values disable that axis.
// M1 enforces MaxInputTokens pre-call in step.LLMCall; MaxOutputTokens,
// MaxUSD, and MaxWallClock are populated into RunStarted so the log is
// self-describing but enforcement lands with T11 / M3.
type Budget struct {
	MaxInputTokens  int64
	MaxOutputTokens int64
	MaxUSD          float64
	MaxWallClock    time.Duration
}
