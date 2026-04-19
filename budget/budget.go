// Package budget defines cost and token budgets and the enforcement logic
// that cancels in-flight LLM streams when a budget trips.
package budget

import "time"

// Budget caps enforced across a run. Zero on any field disables that
// axis.
//
//   - MaxInputTokens is enforced pre-call inside step.LLMCall against
//     an estimate of the upcoming request. Trip emits
//     BudgetExceeded{Where:"pre_call"}.
//   - MaxOutputTokens is enforced mid-stream after every ChunkUsage
//     the provider emits. Trip emits BudgetExceeded{Where:"mid_stream"}.
//   - MaxUSD is enforced mid-stream against the running cumulative
//     cost derived from per-model prices. Trip emits
//     BudgetExceeded{Where:"mid_stream"}.
//   - MaxWallClock is enforced at the agent level: Run wraps ctx with
//     context.WithDeadline(startWall.Add(MaxWallClock)). Trip surfaces
//     as context.DeadlineExceeded and the agent emits
//     BudgetExceeded{Limit:"wall_clock"} + RunFailed{ErrorType:"budget"}.
//
// All trips terminate the run with RunFailed{ErrorType:"budget"}; the
// BudgetExceeded event immediately precedes it in the log.
type Budget struct {
	MaxInputTokens  int64
	MaxOutputTokens int64
	MaxUSD          float64
	MaxWallClock    time.Duration
}
