package starling

import (
	"github.com/jerkeyray/starling/budget"
	"github.com/jerkeyray/starling/internal/cborenc"
)

// Config captures the per-run knobs the user supplies on Agent.
// Every field is optional with a documented default.
type Config struct {
	// Model is the provider-specific model identifier passed through
	// to every LLM call. Required in practice; the adapter will error
	// if empty.
	Model string

	// SystemPrompt is prepended to every conversation and captured
	// verbatim into RunStarted.
	SystemPrompt string

	// Params is the raw provider-specific parameter blob (temperature,
	// top_p, max_tokens, …). Canonical CBOR so the hash in RunStarted
	// is stable across runs with equivalent params.
	Params cborenc.RawMessage

	// MaxTurns caps how many assistant/tool cycles the loop will run.
	// 0 means unlimited — not recommended.
	MaxTurns int
}

// Budget is re-exported from the budget package for callers that want
// a single import path. All four axes are enforced end-to-end:
// MaxInputTokens pre-call (step.LLMCall), MaxOutputTokens and MaxUSD
// mid-stream on every usage chunk (step.LLMCall), MaxWallClock via
// context.WithDeadline at the agent level. Zero on any field disables
// that axis.
type Budget = budget.Budget
