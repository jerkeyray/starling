package step

import (
	"errors"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
)

// Mode selects between a live run (side effects execute and are
// recorded) and a replay run (side effects return pre-recorded values
// without re-running them).
type Mode uint8

const (
	// ModeLive is the default: helpers execute their effect and emit a
	// SideEffectRecorded event capturing the result.
	ModeLive Mode = iota

	// ModeReplay consumes pre-recorded SideEffectRecorded events from
	// Config.Recorded in order, returning the stored values instead of
	// re-running the effect. Intended for the replay verifier (T17).
	ModeReplay
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

	// Mode selects live vs replay. Zero value is ModeLive.
	Mode Mode

	// Recorded is the pre-captured event stream consumed by replay-mode
	// non-determinism helpers. Required when Mode == ModeReplay; ignored
	// otherwise. NewContext panics on ModeReplay with nil Recorded.
	Recorded []event.Event

	// ClockFn overrides the wall-clock source used by step.Now. Defaults
	// to time.Now. Tests inject a fake clock; under ModeReplay it is
	// never invoked (the recorded value is returned).
	ClockFn func() time.Time
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

// ErrReplayMismatch is returned by a non-determinism helper when the
// next SideEffectRecorded in the replay stream doesn't match what was
// expected — a name mismatch, a missing event, or a type-decode
// failure. The replay verifier wraps this into starling.ErrNonDeterminism.
var ErrReplayMismatch = errors.New("step: replay mismatch")
