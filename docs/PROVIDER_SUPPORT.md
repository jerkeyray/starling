# Provider support matrix

Starling's `Provider` interface is vendor-neutral: anything that can
produce a stream of `StreamChunk` values fits. Five in-tree adapters
ship today — OpenAI, Anthropic, Gemini, Amazon Bedrock, and OpenRouter. This page is
the current truth about what each one does and what is intentionally
deferred.

The OpenAI / Anthropic columns below are the most exhaustively tested.
Gemini, Bedrock, and OpenRouter implement the same `Provider` contract;
refer to their packages for adapter-specific knobs.

Anything marked **n/a** reflects a feature the underlying provider
simply doesn't offer; **deferred** means the feature exists upstream
but Starling hasn't wired it yet.

## Supported features

| Feature                          | OpenAI                          | Anthropic                                | Bedrock                                  | Notes |
|----------------------------------|---------------------------------|------------------------------------------|------------------------------------------|-------|
| Streaming responses              | ✅                              | ✅                                       | ✅ (`ConverseStream`)                    | Normalized to `StreamChunk`. |
| Tool calls (custom)              | ✅                              | ✅                                       | ✅                                       | JSON-schema–typed `tool.Tool` round-trips to all providers. MCP server tools can be adapted via `tool/mcp`. |
| Reasoning / thinking text        | ✅ (reasoning summaries)        | ✅ (extended thinking `thinking_delta`)  | ✅ (`reasoningContent`)                  | Emitted as `ReasoningEmitted` with `Sensitive=true`. |
| Thinking signature               | n/a                             | ✅                                       | ✅                                       | Provider signatures are preserved on reasoning chunks when returned. |
| Redacted thinking                | n/a                             | ✅                                       | ✅                                       | `ReasoningEmitted{Redacted: true}` carries opaque payload + signature. |
| Prompt caching                   | ✅ (automatic, read-only usage) | ✅ (explicit via `cache_control`)        | ✅ (usage counters)                      | Usage reports `CacheReadTokens` / `CacheCreateTokens` when providers expose them. |
| `tool_choice`                    | ✅ (`auto`/`required`/`none`/name) | ✅ (`auto`/`any`/`none`/name)         | ✅ (`auto`/`any`/name; no `none`)        | Bedrock Converse has no `none` tool-choice union. |
| `stop_sequences`                 | ✅                              | ✅                                       | ✅                                       | `Request.StopSequences`. OpenAI collapses single-element to a string for API compliance. |
| `top_k`                          | n/a                             | ✅                                       | ✅ (`additionalModelRequestFields.top_k`) | `Request.TopK`. OpenAI ignores. |
| `max_output_tokens`              | ✅ (`max_completion_tokens`)    | ✅ (required; defaults to 4096)          | ✅ (`maxTokens`)                         | `Request.MaxOutputTokens`. |
| `temperature` / `top_p`          | ✅                              | ✅                                       | ✅                                       | Routed through `Request.Params` where not first-class. |
| Usage + cost reporting           | ✅                              | ✅                                       | ✅                                       | `UsageUpdate.{Input,Output,CacheCreate,CacheRead}Tokens` populated where available. |
| `ProviderReqID` capture          | ✅ (`request-id` header)        | ✅ (`request-id` header + fallback to `message.id`) | ✅ (AWS request metadata)       | Recorded on `AssistantMessageCompleted`. |
| Raw-response hash                | ✅ (BLAKE3 over SSE bytes)      | ✅ (BLAKE3 over SSE JSON bytes)          | ✅ (BLAKE3 over SDK event JSON)          | `ChunkEnd.RawResponseHash`. |

## Deferred (tracked for follow-up tasks)

| Feature                                 | Status                                                      |
|-----------------------------------------|-------------------------------------------------------------|
| Bedrock OpenAI-compatible Chat Completions / Responses endpoints | Deferred — use `provider/openai` with a Bedrock-compatible base URL or add a thin wrapper later. |
| Server-hosted tools (`web_search`, `code_execution`, `bash`, `text_editor`, `computer_use`, `memory`, `tool_search`) | Deferred — needs a new `ChunkKind` family for inline server-tool result blocks. |
| Image / document / video / `container_upload` input blocks | Deferred — add when a caller needs multimodal input. |
| Citations (`citations_delta`)           | Deferred — five citation-location variants deserve a unified cross-provider abstraction. |
| Structured outputs (`response_format` / Bedrock `outputConfig`) | Deferred — design a unified shape once providers have stable compatible forms. |
| `service_tier`, `inference_geo`, `container`, `metadata.user_id`, Bedrock guardrail trace | Route through provider-specific request fields where supported; promote to first-class fields if broadly adopted. |
| MCP resources / prompts / sampling      | Deferred — `tool/mcp` intentionally adapts MCP tools only. |

## Amazon Bedrock

The Bedrock adapter uses the AWS SDK for Go v2 and the native
`ConverseStream` API. It accepts the same model identifiers Bedrock
accepts for Converse, including foundation model IDs, inference
profiles, prompt ARNs, and provisioned throughput ARNs.

```go
awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
if err != nil {
	return err
}
prov, err := bedrock.New(bedrock.WithAWSConfig(awsCfg))
```

`Request.Params` supports Bedrock-specific extras:
`temperature`, `topP`/`top_p`, `additionalModelRequestFields`,
`additionalModelResponseFieldPaths`, `requestMetadata`,
`performanceConfig`, `serviceTier`, `promptVariables`,
`outputConfig`, and `guardrailConfig`. Unknown keys are rejected so
misspelled Bedrock fields do not silently no-op.

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
