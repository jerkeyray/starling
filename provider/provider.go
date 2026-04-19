// Package provider defines the Provider interface and the normalized stream
// chunk types every LLM adapter produces.
//
// Adapters (provider/openai, provider/anthropic, ...) translate vendor
// wire formats into a common stream of StreamChunk values. Token counting,
// budget enforcement, and event emission happen in the step package, not
// here — providers stay pure normalization.
package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jerkeyray/starling/internal/cborenc"
)

// Info describes a Provider instance. Captured into RunStarted at run
// start so the log self-describes which backend produced the events.
type Info struct {
	ID         string // stable provider identifier, e.g. "openai", "anthropic"
	APIVersion string // vendor-specific wire-format version
}

// Provider is an LLM backend adapter. Implementations translate a Request
// into a streaming response, normalized to StreamChunk values.
type Provider interface {
	// Info returns stable identifying metadata for this provider instance.
	// Safe to call concurrently; must not hit the network.
	Info() Info

	// Stream opens a streaming completion for req. The returned
	// EventStream is drained with Next; the caller must Close it when
	// done. An error returned from Stream indicates the stream failed to
	// open; errors mid-stream surface from Next.
	Stream(ctx context.Context, req *Request) (EventStream, error)
}

// Role identifies the sender of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// String returns r unchanged. It exists so Role satisfies fmt.Stringer for
// log dumps; unknown values pass through as-is.
func (r Role) String() string { return string(r) }

// Request is the input to Provider.Stream. All fields are read-only for
// the duration of the call; adapters must not mutate them.
type Request struct {
	Model        string
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDefinition
	Params       cborenc.RawMessage // provider-specific (temperature, top_p, ...)
}

// Message is one entry in the conversation history supplied to the model.
//
// ToolUses is set when Role=RoleAssistant and the assistant planned one or
// more tool calls in a prior turn. ToolResult is set when Role=RoleTool
// and the message carries the result of a tool call back to the model.
type Message struct {
	Role       Role
	Content    string
	ToolUses   []ToolUse
	ToolResult *ToolResult
}

// ToolDefinition is what the caller advertises to the model: the set of
// tools it may request. Schema is the JSON Schema describing the tool's
// input arguments (same bytes as tool.Tool.Schema()).
type ToolDefinition struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolUse is a tool call the assistant emitted on a prior turn. Args holds
// the JSON arguments produced by the model.
type ToolUse struct {
	CallID string
	Name   string
	Args   json.RawMessage
}

// ToolResult is the result of a tool call being fed back to the model on
// a RoleTool message. Content is stringly-typed because both OpenAI and
// Anthropic expect a string on the wire; structured results should be
// JSON-encoded by the caller.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Response is the aggregated outcome of a streaming completion. The step
// package assembles this from the chunk stream and returns it to callers
// so they don't have to re-parse the event log.
//
// ToolUses carries the planned tool calls with Args as JSON — callers
// (typically the agent loop) pass CallID/Name/Args through to
// step.CallTool, which handles the JSON→CBOR conversion for the event
// log.
type Response struct {
	Text            string
	ToolUses        []ToolUse
	StopReason      string
	Usage           UsageUpdate
	CostUSD         float64
	RawResponseHash []byte
	ProviderReqID   string
}

// EventStream delivers StreamChunks from a provider. Next returns io.EOF
// when the stream is complete. Callers must Close the stream when done,
// whether or not EOF was reached.
type EventStream interface {
	Next(ctx context.Context) (StreamChunk, error)
	Close() error
}

// StreamChunk is a single normalized event in a provider stream. Only the
// fields relevant to Kind are populated; the rest are zero.
type StreamChunk struct {
	Kind ChunkKind

	// Text is set on ChunkText and ChunkReasoning.
	Text string

	// ToolUse is set on ChunkToolUseStart, ChunkToolUseDelta, ChunkToolUseEnd.
	ToolUse *ToolUseChunk

	// Usage is set on ChunkUsage.
	Usage *UsageUpdate

	// StopReason, RawResponseHash, and ProviderReqID are set on ChunkEnd.
	//
	// RawResponseHash is the provider-contract chain-of-custody field:
	// adapters must populate it with a BLAKE3-256 digest (32 bytes) over
	// the unmodified SDK-level response bytes so a later reader can prove
	// the event log faithfully represents what the provider returned.
	// step.LLMCall accepts an empty value to stay tolerant of
	// third-party adapters, but the in-tree adapters must set it.
	StopReason      string
	RawResponseHash []byte
	ProviderReqID   string
}

// ChunkKind discriminates StreamChunk variants.
type ChunkKind uint8

const (
	ChunkText ChunkKind = iota + 1
	ChunkReasoning
	ChunkToolUseStart
	ChunkToolUseDelta
	ChunkToolUseEnd
	ChunkUsage
	ChunkEnd
)

// String returns the canonical name of k. Unknown kinds render as
// "ChunkKind(<n>)" so log dumps remain readable.
func (k ChunkKind) String() string {
	switch k {
	case ChunkText:
		return "ChunkText"
	case ChunkReasoning:
		return "ChunkReasoning"
	case ChunkToolUseStart:
		return "ChunkToolUseStart"
	case ChunkToolUseDelta:
		return "ChunkToolUseDelta"
	case ChunkToolUseEnd:
		return "ChunkToolUseEnd"
	case ChunkUsage:
		return "ChunkUsage"
	case ChunkEnd:
		return "ChunkEnd"
	}
	return fmt.Sprintf("ChunkKind(%d)", uint8(k))
}

// ToolUseChunk describes a tool-call fragment inside a stream.
//
//   - ChunkToolUseStart: CallID and Name are set; ArgsDelta is empty.
//   - ChunkToolUseDelta: CallID and ArgsDelta are set; Name is empty.
//   - ChunkToolUseEnd:   CallID is set; Name and ArgsDelta are empty.
//
// Callers buffer ArgsDelta values between Start and End to reconstruct the
// complete argument JSON.
type ToolUseChunk struct {
	CallID    string
	Name      string
	ArgsDelta string
}

// UsageUpdate reports token usage. Semantics differ by provider:
//
//   - OpenAI-family (include_usage: true): delivered once on the terminal
//     chunk with final totals.
//   - Anthropic: delivered on message_start (input tokens) and cumulatively
//     on message_delta (output tokens).
type UsageUpdate struct {
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

