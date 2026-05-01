# Reference: events

The complete event surface. Source of truth lives in
[`event/event.go`](../../event/event.go) (the `Kind` enum) and
[`event/types.go`](../../event/types.go) (the per-kind payload
structs). This page is a flat human-readable index. Field tags shown
are the on-disk CBOR keys and the JSON projection key (used by the
inspector and `event.ToJSON`); they are byte-stable across releases
within a single `event.SchemaVersion`.

A run's chain always starts with `RunStarted` (Seq=1, no PrevHash)
and ends with one of `RunCompleted`, `RunFailed`, `RunCancelled`.
Every other kind is interior. `RunResumed` is a non-terminal seam
written by `Agent.Resume` when a successor process picks up a
crashed run.

## Kind enum (`event.Kind`, `uint8`)

| ID | Name | Family | Terminal? |
| ---: | --- | --- | --- |
| 1  | `RunStarted` | lifecycle | no (start) |
| 2  | `UserMessageAppended` | message | no |
| 3  | `TurnStarted` | lifecycle | no |
| 4  | `ReasoningEmitted` | message | no |
| 5  | `AssistantMessageCompleted` | message | no |
| 6  | `ToolCallScheduled` | tool | no |
| 7  | `ToolCallCompleted` | tool | no |
| 8  | `ToolCallFailed` | tool | no |
| 9  | `SideEffectRecorded` | tool | no |
| 10 | `BudgetExceeded` | budget | no |
| 11 | `ContextTruncated` | budget | no |
| 12 | `RunCompleted` | terminal | **yes** (success) |
| 13 | `RunFailed` | terminal | **yes** (failure) |
| 14 | `RunCancelled` | terminal | **yes** (ctx) |
| 15 | `RunResumed` | lifecycle | no (seam) |

`Kind.String()` returns the canonical name; `Kind.IsTerminal()` is
true for 12, 13, 14.

## Per-kind payloads

Notation: `Required` means the field is always populated; `Optional`
means it may be the zero value (the corresponding CBOR/JSON tag has
`omitempty`).

---

### `RunStarted` (Kind=1)

The run's preamble. Pins everything needed for replay and audit:
provider, model, tool registry, budget, system prompt, and the
schema version that determines how subsequent events decode.

| Field | Type | Tag | Notes |
| --- | --- | --- | --- |
| `SchemaVersion` | `uint32` | `schema_version` | Required. Resume/Replay refuse runs whose version this binary doesn't know. |
| `Goal` | `string` | `goal` | Required. The argument to `Agent.Run`. |
| `ProviderID` | `string` | `provider_id` | Required. From `Provider.Info().ID`. |
| `ModelID` | `string` | `model_id` | Required. From `Config.Model`. |
| `APIVersion` | `string` | `api_version` | Required. From `Provider.Info().APIVersion`. |
| `ParamsHash` | `[]byte` | `params_hash` | BLAKE3-32 of `Config.Params`. |
| `Params` | `cborenc.RawMessage` | `params` | Verbatim `Config.Params`. |
| `SystemPromptHash` | `[]byte` | `system_prompt_hash` | BLAKE3-32 of `SystemPrompt`. |
| `SystemPrompt` | `string` | `system_prompt` | Verbatim. |
| `ToolRegistryHash` | `[]byte` | `tool_registry_hash` | BLAKE3-32 of the canonical-CBOR-encoded `ToolSchemas`. |
| `ToolSchemas` | `[]ToolSchemaRef` | `tool_schemas` | Per-tool `{name, schema_hash}`. |
| `Budget` | `*BudgetLimits` | `budget,omitempty` | Mirrors `Agent.Budget`. |
| `StarlingVersion` | `string` | `starling_version,omitempty` | Linked module version. |
| `AppVersion` | `string` | `app_version,omitempty` | From `Config.AppVersion`. |

### `UserMessageAppended` (Kind=2)

Records a user message injected mid-run. Usually emitted by `Resume`
when called with a non-empty `extraMessage`.

| Field | Type | Tag |
| --- | --- | --- |
| `Content` | `string` | `content` |

### `TurnStarted` (Kind=3)

Marks the beginning of an LLM turn. The same `TurnID` is referenced
by every event produced inside the turn (assistant message, tool
calls, reasoning).

| Field | Type | Tag |
| --- | --- | --- |
| `TurnID` | `string` | `turn_id` |
| `PromptHash` | `[]byte` | `prompt_hash` |
| `InputTokens` | `int64` | `input_tokens` |

### `ReasoningEmitted` (Kind=4)

Provider-supplied reasoning content (Anthropic extended thinking,
OpenAI reasoning models). `Signature` and `Redacted` exist for
Anthropic round-trip fidelity: the server requires them replayed
verbatim on subsequent turns.

| Field | Type | Tag |
| --- | --- | --- |
| `TurnID` | `string` | `turn_id` |
| `Content` | `string` | `content` |
| `Sensitive` | `bool` | `sensitive` |
| `Signature` | `[]byte` | `signature,omitempty` |
| `Redacted` | `bool` | `redacted,omitempty` |

### `AssistantMessageCompleted` (Kind=5)

The successful end-of-turn payload. Carries both the assistant text
and the model's planned tool uses, plus the usage update from the
provider.

| Field | Type | Tag |
| --- | --- | --- |
| `TurnID` | `string` | `turn_id` |
| `Text` | `string` | `text` |
| `ToolUses` | `[]PlannedToolUse` | `tool_uses,omitempty` |
| `StopReason` | `string` | `stop_reason` |
| `InputTokens` | `int64` | `input_tokens` |
| `OutputTokens` | `int64` | `output_tokens` |
| `CacheReadTokens` | `int64` | `cache_read_tokens,omitempty` |
| `CacheCreateTokens` | `int64` | `cache_create_tokens,omitempty` |
| `CostUSD` | `float64` | `cost_usd` |
| `RawResponseHash` | `[]byte` | `raw_response_hash` |
| `ProviderRequestID` | `string` | `provider_request_id,omitempty` |

### `ToolCallScheduled` (Kind=6)

Emitted just before invoking a tool. `Attempt` starts at 1; retries
emit a fresh `ToolCallScheduled` with `Attempt+1` rather than
mutating the prior event.

| Field | Type | Tag |
| --- | --- | --- |
| `CallID` | `string` | `call_id` |
| `TurnID` | `string` | `turn_id` |
| `ToolName` | `string` | `tool` |
| `Args` | `cborenc.RawMessage` | `args` |
| `Attempt` | `uint32` | `attempt` |
| `IdempKey` | `string` | `idemp_key,omitempty` |

### `ToolCallCompleted` (Kind=7)

| Field | Type | Tag |
| --- | --- | --- |
| `CallID` | `string` | `call_id` |
| `Result` | `cborenc.RawMessage` | `result` |
| `DurationMs` | `int64` | `duration_ms` |
| `Attempt` | `uint32` | `attempt` |

### `ToolCallFailed` (Kind=8)

`Final` reports whether retries are exhausted; readers can stop
scanning forward when true.

| Field | Type | Tag |
| --- | --- | --- |
| `CallID` | `string` | `call_id` |
| `Error` | `string` | `error` |
| `ErrorType` | `ToolErrorType` | `error_type` |
| `DurationMs` | `int64` | `duration_ms` |
| `Attempt` | `uint32` | `attempt` |
| `Final` | `bool` | `final,omitempty` |

`ToolErrorType`:

| Value | When |
| --- | --- |
| `panic` | The tool function panicked; wrapped via `tool.ErrPanicked`. |
| `cancelled` | `ctx` cancelled before completion. |
| `tool` | The tool returned a non-nil error. |

### `SideEffectRecorded` (Kind=9)

A non-deterministic value committed to the chain so replay can
return the recorded value instead of re-running the effect. Emitted
by `step.Now`, `step.Random`, and `step.SideEffect`.

| Field | Type | Tag |
| --- | --- | --- |
| `Name` | `string` | `name` |
| `Value` | `cborenc.RawMessage` | `value` |

### `BudgetExceeded` (Kind=10)

| Field | Type | Tag |
| --- | --- | --- |
| `Limit` | `BudgetLimit` | `limit` |
| `Cap` | `float64` | `cap` |
| `Actual` | `float64` | `actual` |
| `Where` | `BudgetWhere` | `where` |
| `TurnID` | `string` | `turn_id,omitempty` |
| `CallID` | `string` | `call_id,omitempty` |
| `PartialText` | `string` | `partial_text,omitempty` |
| `PartialTokens` | `int64` | `partial_tokens,omitempty` |

`BudgetLimit`: `input_tokens`, `output_tokens`, `usd`, `wall_clock`.
`BudgetWhere`: `pre_call`, `mid_stream`.

### `ContextTruncated` (Kind=11)

Conversation-history trimming. `Strategy` is e.g. `drop_oldest` or
`summarize`.

| Field | Type | Tag |
| --- | --- | --- |
| `Strategy` | `string` | `strategy` |
| `TokensBefore` | `int64` | `tokens_before` |
| `TokensAfter` | `int64` | `tokens_after` |
| `MessagesBefore` | `uint32` | `messages_before` |
| `MessagesAfter` | `uint32` | `messages_after` |

### `RunCompleted` (Kind=12) - terminal, success

| Field | Type | Tag |
| --- | --- | --- |
| `FinalText` | `string` | `final_text` |
| `TurnCount` | `uint32` | `turn_count` |
| `ToolCallCount` | `uint32` | `tool_call_count` |
| `TotalCostUSD` | `float64` | `total_cost_usd` |
| `TotalInputTokens` | `int64` | `total_input_tokens` |
| `TotalOutputTokens` | `int64` | `total_output_tokens` |
| `DurationMs` | `int64` | `duration_ms` |
| `MerkleRoot` | `[]byte` | `merkle_root` |

### `RunFailed` (Kind=13) - terminal, failure

| Field | Type | Tag |
| --- | --- | --- |
| `Error` | `string` | `error` |
| `ErrorType` | `RunErrorType` | `error_type` |
| `MerkleRoot` | `[]byte` | `merkle_root` |
| `DurationMs` | `int64` | `duration_ms` |

`RunErrorType`: `budget`, `max_turns`, `tool`, `provider`, `internal`.

### `RunCancelled` (Kind=14) - terminal, cancellation

| Field | Type | Tag |
| --- | --- | --- |
| `Reason` | `string` | `reason` (e.g. `context_canceled`, `user_cancel`) |
| `MerkleRoot` | `[]byte` | `merkle_root` |
| `DurationMs` | `int64` | `duration_ms` |

### `RunResumed` (Kind=15)

Non-terminal seam written by `Agent.Resume`. Events before it were
written by the original process; events after, by the resumer.

| Field | Type | Tag |
| --- | --- | --- |
| `AtSeq` | `uint64` | `at_seq` (seq of last pre-resume event) |
| `ExtraMessage` | `string` | `extra_message,omitempty` |
| `ReissueTools` | `bool` | `reissue_tools` |
| `PendingCalls` | `uint32` | `pending_calls` |

## Provider `Annotations` keys

`provider.Message.Annotations` is a `map[string]any` carrying
vendor-specific per-message metadata that doesn't fit the
provider-neutral `Message` shape. Currently consumed by the
Anthropic adapter only.

| Key | Read by | Expected shape | Effect |
| --- | --- | --- | --- |
| `cache_control` | `provider/anthropic` | `{"type":"ephemeral","ttl":"5m"|"1h"}` | Adds an Anthropic prompt-cache breakpoint to the message. Other providers ignore. |

Extensions are by mutual agreement between the adapter and the
caller; nothing in the runtime parses unknown keys.

## See also

- [`event/encoding.go`](../../event/encoding.go) - `Marshal`,
  `EncodePayload`, the typed `As*` accessors.
- [reference/save-file.md](save-file.md) - how these events lay out
  on SQLite disk.
- [mental-model.md](../mental-model.md) for the lifecycle
  ("RunStarted → TurnStarted → ... → terminal") in narrative form.
