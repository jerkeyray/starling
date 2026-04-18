package step

import (
	"errors"

	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
)

// Config captures the static dependencies a step.Context needs across a
// run. The agent loop builds one at run start and never mutates it.
//
// Log and RunID are required and validated by NewContext. Provider and
// Tools are only checked when the corresponding helper is invoked:
// LLMCall panics if Provider is nil; CallTool returns ErrToolNotFound
// if Tools is nil. Budget is optional — a zero-valued BudgetConfig
// disables pre-call input-token enforcement.
type Config struct {
	Log      eventlog.EventLog
	RunID    string
	Provider provider.Provider
	Tools    *Registry
	Budget   BudgetConfig
}

// BudgetConfig holds the subset of budget caps enforced by the step
// package in M1. The full Budget struct (wall-clock, USD, output) lives
// in the budget package and arrives in T11.
type BudgetConfig struct {
	// MaxInputTokens caps per-call input tokens. 0 means unlimited.
	MaxInputTokens int64
}

// ErrBudgetExceeded is returned by LLMCall when the pre-call input-token
// estimate exceeds BudgetConfig.MaxInputTokens. The matching
// BudgetExceeded event is emitted before the error is returned.
// Callers (typically the agent loop) wrap this into RunFailed.
var ErrBudgetExceeded = errors.New("step: budget exceeded")

// ErrToolNotFound is returned by CallTool when the requested tool name
// is not in the Registry. A ToolCallFailed event with
// ErrorType="tool" is emitted before the error is returned.
var ErrToolNotFound = errors.New("step: tool not found")
