package starling

import (
	"log/slog"
	"time"

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

	// RequireRawResponseHash fails any turn whose ChunkEnd lacks a
	// 32-byte hash.
	RequireRawResponseHash bool

	// AppVersion identifies the caller's application build and is
	// stamped into RunStarted alongside the Starling library version.
	// Optional; left blank when unset.
	AppVersion string

	// EmitTimeout bounds each event-log Append the agent issues under
	// context.WithoutCancel (terminal events, tool failures during
	// cancellation). Zero disables the bound; set this when a hung
	// backend must not block shutdown.
	EmitTimeout time.Duration

	// SkipSchemaCheck disables the pre-flight schema-version check that
	// Run, Resume, and Replay run against the event log. Reserved for
	// tests and tooling that intentionally point at a database older
	// than the binary.
	SkipSchemaCheck bool

	// Logger receives structured slog records covering the run lifecycle:
	// RunStarted, per-turn start, budget trips, tool retries, and the
	// terminal event. Every record carries a "run_id" attribute; per-turn
	// and per-tool records add "turn_id" / "call_id".
	//
	// The event log remains the source of truth for auditing — Logger is
	// a side-channel trace for operators watching live runs. If nil, the
	// process-wide slog.Default() is used; pass a discard logger to
	// silence library output entirely.
	Logger *slog.Logger
}

// Budget is re-exported from the budget package for callers that want
// a single import path. All four axes are enforced end-to-end:
// MaxInputTokens pre-call (step.LLMCall), MaxOutputTokens and MaxUSD
// mid-stream on every usage chunk (step.LLMCall), MaxWallClock via
// context.WithDeadline at the agent level. Zero on any field disables
// that axis.
type Budget = budget.Budget
