package event

import "github.com/jerkeyray/starling/internal/cborenc"

// ---------------------------------------------------------------------------
// Shared helper types
// ---------------------------------------------------------------------------

// ToolSchemaRef pins a tool name to the hash of its JSON Schema at run start.
// Used inside RunStarted so a later replay can detect silent tool-schema
// changes.
type ToolSchemaRef struct {
	Name       string `cbor:"name" json:"name"`
	SchemaHash []byte `cbor:"schema_hash" json:"schema_hash"`
}

// BudgetLimits mirrors the Budget struct the user set on Agent, captured into
// RunStarted so the log self-describes the limits enforcement was run under.
// All fields are optional; zero = no limit on that axis.
type BudgetLimits struct {
	MaxInputTokens  int64   `cbor:"max_input_tokens,omitempty" json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64   `cbor:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	MaxUSD          float64 `cbor:"max_usd,omitempty" json:"max_usd,omitempty"`
	MaxWallClockMs  int64   `cbor:"max_wall_clock_ms,omitempty" json:"max_wall_clock_ms,omitempty"`
}

// PlannedToolUse describes a tool invocation the assistant requested during a
// turn. Args hold the raw CBOR-encoded argument object so downstream events
// can reference them without re-encoding.
type PlannedToolUse struct {
	CallID   string             `cbor:"call_id" json:"call_id"`
	ToolName string             `cbor:"tool" json:"tool"`
	Args     cborenc.RawMessage `cbor:"args" json:"args"`
}

// ---------------------------------------------------------------------------
// 1. RunStarted
// ---------------------------------------------------------------------------

// RunStarted is the first event of every run. It pins the schema version,
// goal, provider, model, params, system prompt, tool registry, and budget so
// the run is self-describing for replay and audit.
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
}

// ---------------------------------------------------------------------------
// 2. UserMessageAppended
// ---------------------------------------------------------------------------

// UserMessageAppended records a new user message injected into an in-progress
// run (typically during Resume).
type UserMessageAppended struct {
	Content string `cbor:"content" json:"content"`
}

// ---------------------------------------------------------------------------
// 3. TurnStarted
// ---------------------------------------------------------------------------

// TurnStarted marks the beginning of an LLM turn, carrying the prompt hash
// and input-token count computed pre-call.
type TurnStarted struct {
	TurnID      string `cbor:"turn_id" json:"turn_id"`
	PromptHash  []byte `cbor:"prompt_hash" json:"prompt_hash"`
	InputTokens int64  `cbor:"input_tokens" json:"input_tokens"`
}

// ---------------------------------------------------------------------------
// 4. ReasoningEmitted
// ---------------------------------------------------------------------------

// ReasoningEmitted records provider-supplied reasoning (e.g. Anthropic
// extended thinking). Sensitive=true flags content the caller may want to
// redact on display.
//
// Signature carries Anthropic's per-block integrity signature: on a
// standard thinking block it is produced by the trailing signature_delta
// event, on a redacted_thinking block it accompanies the opaque payload.
// The signature must be replayed verbatim back to Anthropic on subsequent
// turns for the server to accept the assistant message, so it is
// recorded here to keep replays byte-faithful.
//
// Redacted=true marks a redacted_thinking block: Content holds the opaque
// payload the server returned (not plaintext reasoning) and must also
// round-trip unchanged. OpenAI adapters never set Signature or Redacted.
type ReasoningEmitted struct {
	TurnID    string `cbor:"turn_id" json:"turn_id"`
	Content   string `cbor:"content" json:"content"`
	Sensitive bool   `cbor:"sensitive" json:"sensitive"`
	Signature []byte `cbor:"signature,omitempty" json:"signature,omitempty"`
	Redacted  bool   `cbor:"redacted,omitempty" json:"redacted,omitempty"`
}

// ---------------------------------------------------------------------------
// 5. AssistantMessageCompleted
// ---------------------------------------------------------------------------

// AssistantMessageCompleted is the successful terminal event of a turn. It
// captures the full assistant output plus authoritative token counts, cost,
// and a hash of the raw provider response.
type AssistantMessageCompleted struct {
	TurnID            string             `cbor:"turn_id" json:"turn_id"`
	Text              string             `cbor:"text" json:"text"`
	ToolUses          []PlannedToolUse   `cbor:"tool_uses,omitempty" json:"tool_uses,omitempty"`
	StopReason        string             `cbor:"stop_reason" json:"stop_reason"`
	InputTokens       int64              `cbor:"input_tokens" json:"input_tokens"`
	OutputTokens      int64              `cbor:"output_tokens" json:"output_tokens"`
	CacheReadTokens   int64              `cbor:"cache_read_tokens,omitempty" json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int64              `cbor:"cache_create_tokens,omitempty" json:"cache_create_tokens,omitempty"`
	CostUSD           float64            `cbor:"cost_usd" json:"cost_usd"`
	RawResponseHash   []byte             `cbor:"raw_response_hash" json:"raw_response_hash"`
	ProviderRequestID string             `cbor:"provider_request_id,omitempty" json:"provider_request_id,omitempty"`
}

// ---------------------------------------------------------------------------
// 6. ToolCallScheduled
// ---------------------------------------------------------------------------

// ToolCallScheduled is emitted immediately before a tool is invoked. Attempt
// starts at 1 and increments on retries; IdempKey is optional but recommended
// for non-idempotent side effects.
type ToolCallScheduled struct {
	CallID   string             `cbor:"call_id" json:"call_id"`
	TurnID   string             `cbor:"turn_id" json:"turn_id"`
	ToolName string             `cbor:"tool" json:"tool"`
	Args     cborenc.RawMessage `cbor:"args" json:"args"`
	Attempt  uint32             `cbor:"attempt" json:"attempt"`
	IdempKey string             `cbor:"idemp_key,omitempty" json:"idemp_key,omitempty"`
}

// ---------------------------------------------------------------------------
// 7. ToolCallCompleted
// ---------------------------------------------------------------------------

// ToolCallCompleted captures a successful tool invocation. The CallID and
// Attempt must match the ToolCallScheduled that initiated it.
type ToolCallCompleted struct {
	CallID     string             `cbor:"call_id" json:"call_id"`
	Result     cborenc.RawMessage `cbor:"result" json:"result"`
	DurationMs int64              `cbor:"duration_ms" json:"duration_ms"`
	Attempt    uint32             `cbor:"attempt" json:"attempt"`
}

// ---------------------------------------------------------------------------
// 8. ToolCallFailed
// ---------------------------------------------------------------------------

// ToolCallFailed captures a failed tool invocation. ErrorType classifies the
// failure (e.g. "panic", "timeout", "schema_violation").
type ToolCallFailed struct {
	CallID     string `cbor:"call_id" json:"call_id"`
	Error      string `cbor:"error" json:"error"`
	ErrorType  string `cbor:"error_type" json:"error_type"`
	DurationMs int64  `cbor:"duration_ms" json:"duration_ms"`
	Attempt    uint32 `cbor:"attempt" json:"attempt"`
}

// ---------------------------------------------------------------------------
// 9. SideEffectRecorded
// ---------------------------------------------------------------------------

// SideEffectRecorded captures a non-deterministic value consumed by the agent
// loop — wall clock readings (step.Now), random draws (step.Random), or
// user-supplied side effects (step.SideEffect). On replay the recorded Value
// is returned instead of re-running the effect.
type SideEffectRecorded struct {
	Name  string             `cbor:"name" json:"name"`
	Value cborenc.RawMessage `cbor:"value" json:"value"`
}

// ---------------------------------------------------------------------------
// 10. BudgetExceeded
// ---------------------------------------------------------------------------

// BudgetExceeded is emitted when the runtime cancels work because a budget
// cap was reached. Limit identifies which cap ("input_tokens", "output_tokens",
// "usd", "wall_clock"); Where distinguishes pre-call vs mid-stream enforcement.
type BudgetExceeded struct {
	Limit         string  `cbor:"limit" json:"limit"`
	Cap           float64 `cbor:"cap" json:"cap"`
	Actual        float64 `cbor:"actual" json:"actual"`
	Where         string  `cbor:"where" json:"where"`
	TurnID        string  `cbor:"turn_id,omitempty" json:"turn_id,omitempty"`
	CallID        string  `cbor:"call_id,omitempty" json:"call_id,omitempty"`
	PartialText   string  `cbor:"partial_text,omitempty" json:"partial_text,omitempty"`
	PartialTokens int64   `cbor:"partial_tokens,omitempty" json:"partial_tokens,omitempty"`
}

// ---------------------------------------------------------------------------
// 11. ContextTruncated
// ---------------------------------------------------------------------------

// ContextTruncated records context-window management trimming prior messages.
// Strategy names the approach used (e.g. "drop_oldest", "summarize").
type ContextTruncated struct {
	Strategy        string `cbor:"strategy" json:"strategy"`
	TokensBefore    int64  `cbor:"tokens_before" json:"tokens_before"`
	TokensAfter     int64  `cbor:"tokens_after" json:"tokens_after"`
	MessagesBefore  uint32 `cbor:"messages_before" json:"messages_before"`
	MessagesAfter   uint32 `cbor:"messages_after" json:"messages_after"`
}

// ---------------------------------------------------------------------------
// 12. RunCompleted (terminal)
// ---------------------------------------------------------------------------

// RunCompleted is the successful terminal event of a run. MerkleRoot commits
// to every event before it — tampering with any prior event breaks the root.
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

// ---------------------------------------------------------------------------
// 13. RunFailed (terminal)
// ---------------------------------------------------------------------------

// RunFailed is the failure terminal event. ErrorType classifies the failure.
type RunFailed struct {
	Error      string `cbor:"error" json:"error"`
	ErrorType  string `cbor:"error_type" json:"error_type"`
	MerkleRoot []byte `cbor:"merkle_root" json:"merkle_root"`
	DurationMs int64  `cbor:"duration_ms" json:"duration_ms"`
}

// ---------------------------------------------------------------------------
// 14. RunCancelled (terminal)
// ---------------------------------------------------------------------------

// RunCancelled is the cancellation terminal event. Reason describes why
// (e.g. "context_canceled", "user_cancel").
type RunCancelled struct {
	Reason     string `cbor:"reason" json:"reason"`
	MerkleRoot []byte `cbor:"merkle_root" json:"merkle_root"`
	DurationMs int64  `cbor:"duration_ms" json:"duration_ms"`
}
