package event

import "github.com/jerkeyray/starling/internal/cborenc"

// ToolSchemaRef pins a tool name to its JSON Schema hash so replay can
// detect silent schema drift.
type ToolSchemaRef struct {
	Name       string `cbor:"name" json:"name"`
	SchemaHash []byte `cbor:"schema_hash" json:"schema_hash"`
}

// BudgetLimits mirrors Agent.Budget so the log self-describes its caps.
// Absent or zero on any field disables that axis (omitempty drops zeros).
type BudgetLimits struct {
	MaxInputTokens  int64   `cbor:"max_input_tokens,omitempty" json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64   `cbor:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	MaxUSD          float64 `cbor:"max_usd,omitempty" json:"max_usd,omitempty"`
	MaxWallClockMs  int64   `cbor:"max_wall_clock_ms,omitempty" json:"max_wall_clock_ms,omitempty"`
}

// PlannedToolUse is a tool invocation requested by the assistant. Args is
// raw CBOR so downstream events can reference it without re-encoding.
type PlannedToolUse struct {
	CallID   string             `cbor:"call_id" json:"call_id"`
	ToolName string             `cbor:"tool" json:"tool"`
	Args     cborenc.RawMessage `cbor:"args" json:"args"`
}

// RunStarted is the first event of every run and pins everything needed
// to make the run self-describing for replay and audit.
type RunStarted struct {
	SchemaVersion    uint32             `cbor:"schema_version" json:"schema_version"`
	Goal             string             `cbor:"goal" json:"goal"`
	ProviderID       string             `cbor:"provider_id" json:"provider_id"`
	ModelID          string             `cbor:"model_id" json:"model_id"`
	APIVersion       string             `cbor:"api_version" json:"api_version"`
	ParamsHash       []byte             `cbor:"params_hash" json:"params_hash"`
	Params           cborenc.RawMessage `cbor:"params" json:"params"`
	SystemPromptHash []byte             `cbor:"system_prompt_hash" json:"system_prompt_hash"`
	SystemPrompt     string             `cbor:"system_prompt" json:"system_prompt"`
	ToolRegistryHash []byte             `cbor:"tool_registry_hash" json:"tool_registry_hash"`
	ToolSchemas      []ToolSchemaRef    `cbor:"tool_schemas" json:"tool_schemas"`
	Budget           *BudgetLimits      `cbor:"budget,omitempty" json:"budget,omitempty"`
	// StarlingVersion comes from the linked starling module; empty when
	// build info is unavailable.
	StarlingVersion string `cbor:"starling_version,omitempty" json:"starling_version,omitempty"`
	AppVersion      string `cbor:"app_version,omitempty" json:"app_version,omitempty"`
}

// UserMessageAppended records a user message injected into an in-progress
// run, typically during Resume.
type UserMessageAppended struct {
	Content string `cbor:"content" json:"content"`
}

// TurnStarted marks the beginning of an LLM turn.
type TurnStarted struct {
	TurnID      string `cbor:"turn_id" json:"turn_id"`
	PromptHash  []byte `cbor:"prompt_hash" json:"prompt_hash"`
	InputTokens int64  `cbor:"input_tokens" json:"input_tokens"`
}

// ReasoningEmitted records provider-supplied reasoning (e.g. Anthropic
// extended thinking). Sensitive flags content the caller may want to redact.
//
// Signature and Redacted exist for Anthropic round-trip fidelity: both must
// be replayed verbatim on subsequent turns or the server rejects the
// assistant message. When Redacted is true, Content holds the opaque
// server payload, not plaintext. OpenAI adapters never set either.
type ReasoningEmitted struct {
	TurnID    string `cbor:"turn_id" json:"turn_id"`
	Content   string `cbor:"content" json:"content"`
	Sensitive bool   `cbor:"sensitive" json:"sensitive"`
	Signature []byte `cbor:"signature,omitempty" json:"signature,omitempty"`
	Redacted  bool   `cbor:"redacted,omitempty" json:"redacted,omitempty"`
}

// AssistantMessageCompleted is the successful terminal event of a turn.
type AssistantMessageCompleted struct {
	TurnID            string           `cbor:"turn_id" json:"turn_id"`
	Text              string           `cbor:"text" json:"text"`
	ToolUses          []PlannedToolUse `cbor:"tool_uses,omitempty" json:"tool_uses,omitempty"`
	StopReason        string           `cbor:"stop_reason" json:"stop_reason"`
	InputTokens       int64            `cbor:"input_tokens" json:"input_tokens"`
	OutputTokens      int64            `cbor:"output_tokens" json:"output_tokens"`
	CacheReadTokens   int64            `cbor:"cache_read_tokens,omitempty" json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int64            `cbor:"cache_create_tokens,omitempty" json:"cache_create_tokens,omitempty"`
	CostUSD           float64          `cbor:"cost_usd" json:"cost_usd"`
	RawResponseHash   []byte           `cbor:"raw_response_hash" json:"raw_response_hash"`
	ProviderRequestID string           `cbor:"provider_request_id,omitempty" json:"provider_request_id,omitempty"`
}

// ToolCallScheduled is emitted immediately before a tool is invoked.
// Attempt starts at 1 and increments on retries.
type ToolCallScheduled struct {
	CallID   string             `cbor:"call_id" json:"call_id"`
	TurnID   string             `cbor:"turn_id" json:"turn_id"`
	ToolName string             `cbor:"tool" json:"tool"`
	Args     cborenc.RawMessage `cbor:"args" json:"args"`
	Attempt  uint32             `cbor:"attempt" json:"attempt"`
	IdempKey string             `cbor:"idemp_key,omitempty" json:"idemp_key,omitempty"`
}

// ToolCallCompleted captures a successful tool invocation. CallID and
// Attempt match the originating ToolCallScheduled.
type ToolCallCompleted struct {
	CallID     string             `cbor:"call_id" json:"call_id"`
	Result     cborenc.RawMessage `cbor:"result" json:"result"`
	DurationMs int64              `cbor:"duration_ms" json:"duration_ms"`
	Attempt    uint32             `cbor:"attempt" json:"attempt"`
}

// ToolCallFailed captures a failed tool invocation. Final reports whether
// retries are exhausted; readers can stop scanning forward when true.
type ToolCallFailed struct {
	CallID     string        `cbor:"call_id" json:"call_id"`
	Error      string        `cbor:"error" json:"error"`
	ErrorType  ToolErrorType `cbor:"error_type" json:"error_type"`
	DurationMs int64         `cbor:"duration_ms" json:"duration_ms"`
	Attempt    uint32        `cbor:"attempt" json:"attempt"`
	Final      bool          `cbor:"final,omitempty" json:"final,omitempty"`
}

// SideEffectRecorded captures a non-deterministic value (step.Now,
// step.Random, step.SideEffect). Replay returns the recorded Value
// instead of re-running the effect.
type SideEffectRecorded struct {
	Name  string             `cbor:"name" json:"name"`
	Value cborenc.RawMessage `cbor:"value" json:"value"`
}

// BudgetExceeded is emitted when a cap is hit. Cap and Actual are
// float64 because the same fields carry token counts, USD, and ms;
// the meaning is determined by Limit.
type BudgetExceeded struct {
	Limit         BudgetLimit `cbor:"limit" json:"limit"`
	Cap           float64     `cbor:"cap" json:"cap"`
	Actual        float64     `cbor:"actual" json:"actual"`
	Where         BudgetWhere `cbor:"where" json:"where"`
	TurnID        string      `cbor:"turn_id,omitempty" json:"turn_id,omitempty"`
	CallID        string      `cbor:"call_id,omitempty" json:"call_id,omitempty"`
	PartialText   string      `cbor:"partial_text,omitempty" json:"partial_text,omitempty"`
	PartialTokens int64       `cbor:"partial_tokens,omitempty" json:"partial_tokens,omitempty"`
}

// ContextTruncated records context-window trimming. Strategy is e.g.
// "drop_oldest" or "summarize".
type ContextTruncated struct {
	Strategy       string `cbor:"strategy" json:"strategy"`
	TokensBefore   int64  `cbor:"tokens_before" json:"tokens_before"`
	TokensAfter    int64  `cbor:"tokens_after" json:"tokens_after"`
	MessagesBefore uint32 `cbor:"messages_before" json:"messages_before"`
	MessagesAfter  uint32 `cbor:"messages_after" json:"messages_after"`
}

// RunCompleted is the success terminal event. MerkleRoot commits to every
// prior event; tampering with any of them breaks the root.
type RunCompleted struct {
	FinalText         string  `cbor:"final_text" json:"final_text"`
	TurnCount         uint32  `cbor:"turn_count" json:"turn_count"`
	ToolCallCount     uint32  `cbor:"tool_call_count" json:"tool_call_count"`
	TotalCostUSD      float64 `cbor:"total_cost_usd" json:"total_cost_usd"`
	TotalInputTokens  int64   `cbor:"total_input_tokens" json:"total_input_tokens"`
	TotalOutputTokens int64   `cbor:"total_output_tokens" json:"total_output_tokens"`
	DurationMs        int64   `cbor:"duration_ms" json:"duration_ms"`
	MerkleRoot        []byte  `cbor:"merkle_root" json:"merkle_root"`
}

// RunFailed is the failure terminal event.
type RunFailed struct {
	Error      string       `cbor:"error" json:"error"`
	ErrorType  RunErrorType `cbor:"error_type" json:"error_type"`
	MerkleRoot []byte       `cbor:"merkle_root" json:"merkle_root"`
	DurationMs int64        `cbor:"duration_ms" json:"duration_ms"`
}

// RunCancelled is the cancellation terminal event. Reason is e.g.
// "context_canceled" or "user_cancel".
type RunCancelled struct {
	Reason     string `cbor:"reason" json:"reason"`
	MerkleRoot []byte `cbor:"merkle_root" json:"merkle_root"`
	DurationMs int64  `cbor:"duration_ms" json:"duration_ms"`
}

// RunResumed is a non-terminal seam marking where (*Agent).Resume re-entered
// a run started by an earlier process. Events before it were written by the
// original process; events after, by the resumer.
//
// AtSeq is the sequence of the last pre-resume event (PrevHash chains over it).
// ExtraMessage, if set, is followed immediately by a UserMessageAppended.
// ReissueTools records the operator's intent (the WithReissueTools option)
// so a reader can verify the resumer behaved as configured; the actual
// outcome is still derivable from subsequent ToolCallScheduled events.
// PendingCalls counts ToolCallScheduled events without a matching
// Completed/Failed at the resume point.
type RunResumed struct {
	AtSeq        uint64 `cbor:"at_seq" json:"at_seq"`
	ExtraMessage string `cbor:"extra_message,omitempty" json:"extra_message,omitempty"`
	ReissueTools bool   `cbor:"reissue_tools" json:"reissue_tools"`
	PendingCalls uint32 `cbor:"pending_calls" json:"pending_calls"`
}
