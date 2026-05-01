# Reference: cost model

How Starling computes per-call USD cost, where the price tables
live, and how to plug in custom or in-house models. Source of
truth: [`budget/prices.go`](../../budget/prices.go).

## Where cost is computed

Per-turn cost is computed in the step layer and stamped onto the
`AssistantMessageCompleted` event:

- The provider's `UsageUpdate` carries `InputTokens`,
  `OutputTokens`, and (for Anthropic-compatible adapters)
  `CacheReadTokens` / `CacheCreateTokens`.
- `budget.CostUSD(model, inTok, outTok)` returns the dollar cost in
  `(input + output) / 1e6 * vendor_rate`.
- `AssistantMessageCompleted.CostUSD` is the per-turn dollar amount.
- `RunCompleted.TotalCostUSD` is the sum across every turn.

`RunResult.TotalCostUSD` mirrors `RunCompleted.TotalCostUSD` for
in-process consumers; the inspector dashboard sums these per run
into the totals strip.

## Built-in price table

Built-in prices live in `budget/prices.go` as USD per million
tokens, sourced from each vendor's pricing page:

| Family | Models priced | Notes |
| --- | --- | --- |
| OpenAI | `gpt-4o`, `gpt-4o-mini`, `gpt-4-turbo`, `gpt-3.5-turbo` | Base input/output. |
| Anthropic | `claude-opus-4-{0,1,5,6,7}`, `claude-sonnet-4-{0,5,6}`, `claude-haiku-{4-5,3-5,3}` | Base rates only - see "Cache" below for the read/write multipliers. |

A model not in the table returns `(0, false)` from `CostUSD` and
emits one stderr line per process: `budget: no price entry for
model "X"; cost_usd will be reported as 0`. The run still runs;
cost columns are zero.

## Registering custom models

`budget.RegisterPricing` lets you add or override entries at
runtime. Useful for in-house models, fine-tunes, OpenAI-compatible
endpoints whose model IDs aren't in the table, or vendor models
released between Starling tags.

```go
import "github.com/jerkeyray/starling/budget"

func init() {
    // Match vendor pricing pages: dollars per million tokens.
    budget.RegisterPricing("acme/llm-7b", 0.20, 0.60)
}
```

Behavior:

- The first call after a `RegisterPricing` returns `(cost, true)`.
- The runtime clears the unknown-model warn-once memo on
  `RegisterPricing`, so a stale stderr warning doesn't outlive the
  registration.
- Concurrent registrations are safe (`sync.RWMutex` under the
  hood).
- Negative or zero rates are accepted; `CostUSD` multiplies through.

## Cache-aware costs (Anthropic)

Anthropic's prompt-cache pricing applies multipliers on top of the
base input rate:

| Token category | Multiplier vs base input |
| --- | --- |
| Cache read (hit) | 0.1× |
| Cache write, ≤ 5 min TTL | 1.25× |
| Cache write, ≤ 1 h TTL | 2.0× |

The base table in `budget/prices.go` only encodes input/output -
cache-tier multipliers are applied at the usage layer where the
cache token counts come in. For consumers, the totals on
`RunCompleted` and the per-turn `CostUSD` already reflect cache
activity; you don't compute the multiplier yourself.

`RunResult.CacheStats` summarises cache activity over the run:

| Field | Meaning |
| --- | --- |
| `Hits` | Turns whose `CacheReadTokens > 0`. |
| `Misses` | Turns with `InputTokens > 0 && CacheReadTokens == 0`. |
| `ReadTokens` | Sum of per-turn cache reads. |
| `CreateTokens` | Sum of per-turn cache writes. |

## Enabling cache breakpoints

Anthropic's cache works on explicit per-message breakpoints. Set
`provider.Message.Annotations["cache_control"]` on the messages you
want cached:

```go
msgs := []provider.Message{
    {Role: provider.RoleSystem, Content: longSystemPrompt,
     Annotations: map[string]any{
         "cache_control": map[string]string{
             "type": "ephemeral", "ttl": "5m",
         },
     }},
    // ...
}
```

Other providers ignore the annotation. See
[reference/events.md - Annotations keys](events.md#provider-annotations-keys).

## Limits and gotchas

- **No prompt-cache for OpenAI yet.** OpenAI's automatic prompt
  caching is reflected in their billing but not separately reported
  to the SDK; Starling can't surface it. Cache fields are zero for
  OpenAI runs.
- **Bedrock cache** is provider-specific; Bedrock's converse stream
  reports cache token counts when the underlying model supports
  it, and they flow through the same `UsageUpdate` fields.
- **Pricing drift.** Vendor prices change. The on-disk
  `cost_usd` is whatever the binary computed at run time; it is
  **not** retroactively recomputed by replay or migration. Old
  recordings reflect old prices.
- **Model name mismatch.** If your provider returns a different
  `ModelID` than `Config.Model` (some compatibility shims do),
  `CostUSD` looks up by `Config.Model`. Use `WithProviderID` /
  `WithBaseURL` plus `RegisterPricing` to align.

## See also

- [reference/events.md](events.md) - the on-disk
  `AssistantMessageCompleted` and terminal-event fields.
- [`budget/prices.go`](../../budget/prices.go) - the built-in
  table and `RegisterPricing` source.
- [Budgets in README](../../README.md#budgets-and-retries) - how
  cost caps are wired into the agent loop.
