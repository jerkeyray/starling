# Provider support matrix

Starling's `Provider` interface is vendor-neutral: anything that can
produce a stream of `StreamChunk` values fits. Two in-tree adapters
ship today — OpenAI and Anthropic. This page is the current truth
about what each one does and what is intentionally deferred.

Anything marked **n/a** reflects a feature the underlying provider
simply doesn't offer; **deferred** means the feature exists upstream
but Starling hasn't wired it yet.

## Supported features

| Feature                          | OpenAI                          | Anthropic                                | Notes |
|----------------------------------|---------------------------------|------------------------------------------|-------|
| Streaming responses              | ✅                              | ✅                                       | SSE under the hood for both. |
| Tool calls (custom)              | ✅                              | ✅                                       | JSON-schema–typed `tool.Tool` round-trips to both shapes. |
| Reasoning / thinking text        | ✅ (reasoning summaries)        | ✅ (extended thinking `thinking_delta`)  | Emitted as `ReasoningEmitted` with `Sensitive=true`. |
| Thinking signature               | n/a                             | ✅                                       | `ReasoningEmitted.Signature` round-trips verbatim so replay is byte-faithful. |
| Redacted thinking                | n/a                             | ✅                                       | `ReasoningEmitted{Redacted: true}` carries opaque payload + signature. |
| Prompt caching                   | ✅ (automatic, read-only usage) | ✅ (explicit via `cache_control`)        | Anthropic: set `Message.Annotations["cache_control"] = {"type":"ephemeral"}`. Usage reports `CacheReadTokens` / `CacheCreateTokens` on both. |
| `tool_choice`                    | ✅ (`auto`/`required`/`none`/name) | ✅ (`auto`/`any`/`none`/name)         | `Request.ToolChoice` — OpenAI adapter maps `any` → `required` for portability. |
| `stop_sequences`                 | ✅                              | ✅                                       | `Request.StopSequences`. OpenAI collapses single-element to a string for API compliance. |
| `top_k`                          | n/a                             | ✅                                       | `Request.TopK`. OpenAI ignores. |
| `max_output_tokens`              | ✅ (`max_completion_tokens`)    | ✅ (required; defaults to 4096)          | `Request.MaxOutputTokens`. Anthropic requires a positive value — adapter substitutes 4096 if zero. |
| `temperature` / `top_p`          | ✅                              | ✅                                       | Routed through `Request.Params` (canonical-CBOR blob). |
| Usage + cost reporting           | ✅                              | ✅                                       | `UsageUpdate.{Input,Output,CacheCreate,CacheRead}Tokens` populated on both. |
| `ProviderReqID` capture          | ✅ (`request-id` header)        | ✅ (`request-id` header + fallback to `message.id`) | Recorded on `AssistantMessageCompleted`. |
| Raw-response hash                | ✅ (BLAKE3 over SSE bytes)      | ✅ (BLAKE3 over SSE JSON bytes)          | `ChunkEnd.RawResponseHash`. |

## Deferred (tracked for follow-up tasks)

| Feature                                 | Status                                                      |
|-----------------------------------------|-------------------------------------------------------------|
| Server-hosted tools (`web_search`, `code_execution`, `bash`, `text_editor`, `computer_use`, `memory`, `tool_search`) | Deferred — needs a new `ChunkKind` family for inline server-tool result blocks. |
| Image / document / `container_upload` input blocks | Deferred — add when a caller needs multimodal input. |
| Citations (`citations_delta`)           | Deferred — five citation-location variants deserve a unified cross-provider abstraction. |
| Structured outputs (`response_format` / `output_config.json_schema`) | Deferred — design a unified shape once both sides have shipped it stably. |
| `service_tier`, `inference_geo`, `container`, `metadata.user_id` | Route through `Request.Params` CBOR escape hatch today; promote to first-class fields if broadly adopted. |
| MCP connectors                          | Deferred to M4 per `temp_notes/M2_PLAN.md`.                 |

## Pricing

`budget/prices.go` ships static per-model $/MTok entries used by
`RunResult.TotalCostUSD`. Unknown models report `$0` and emit a
one-shot warning to stderr — the run is never blocked by a missing
entry. See the source for the current list; update when vendors
publish new pricing.

## Adding a new provider

Implement `provider.Provider` (two methods: `Info`, `Stream`) plus an
`EventStream` that surfaces the normalized `StreamChunk` sequence
documented in `provider/provider.go`. The existing OpenAI and
Anthropic adapters (`provider/openai`, `provider/anthropic`) are the
canonical references — they each isolate a `request.go` / `stream.go`
split so the SSE state machine is readable in isolation.

Replay correctness depends on `ChunkEnd.RawResponseHash` being
deterministic for the same server bytes: hash the on-wire payload,
not SDK struct values.
