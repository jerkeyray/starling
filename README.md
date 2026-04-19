# Starling

**Status:** pre-release. Public API is unstable.

Starling is an event-sourced agent runtime for Go. Every agent run is
recorded as an append-only, BLAKE3-chained, Merkle-rooted event log,
which means every execution is:

- **Replayable** — re-run a goal byte-for-byte from the log and catch
  any drift as `ErrNonDeterminism`.
- **Auditable** — tamper with any prior event and the Merkle root in
  the terminal event no longer matches.
- **Cost-enforceable** — input tokens, output tokens, USD, and
  wall-clock budgets all enforced inline and recorded in the log.

## Install

```sh
go get github.com/jerkeyray/starling
```

Requires Go 1.23+.

## Hello agent

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider/openai"
	"github.com/jerkeyray/starling/tool"
)

type clockOut struct{ UTC string `json:"utc"` }

func main() {
	prov, _ := openai.New(openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	clock := tool.Typed("current_time", "Return the current UTC time.",
		func(_ context.Context, _ struct{}) (clockOut, error) {
			return clockOut{UTC: time.Now().UTC().Format(time.RFC3339)}, nil
		})

	a := &starling.Agent{
		Provider: prov,
		Tools:    []tool.Tool{clock},
		Log:      eventlog.NewInMemory(),
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}
	res, err := a.Run(context.Background(), "What is the current UTC time?")
	if err != nil {
		panic(err)
	}
	fmt.Println(res.FinalText)
}
```

A runnable version (with event-log dump and tamper-evidence check)
lives at [`examples/m1_hello`](./examples/m1_hello).

## Durable log (SQLite)

Swap `NewInMemory` for `NewSQLite` to persist every event to disk.
The log survives process crashes, is hash-chained on insert, and
opens cleanly on restart:

```go
log, err := eventlog.NewSQLite("runs.db")
if err != nil { panic(err) }
defer log.Close()

a := &starling.Agent{
	Provider: prov,
	Tools:    []tool.Tool{clock},
	Log:      log,
	Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
}
```

Pass `":memory:"` as the path for an ephemeral database.

## Replay a run

Once a run is persisted you can re-execute it against the same agent
and verify every emitted event matches the recording:

```go
if err := starling.Replay(ctx, log, runID, a); err != nil {
	if errors.Is(err, starling.ErrNonDeterminism) {
		// A tool output, prompt, or model changed since the original
		// run. Inspect err for the first diverging seq.
	}
	// other errors (log-read, transport, ...) surface verbatim
}
```

See [`docs/REPLAY.md`](./docs/REPLAY.md) for the full cookbook:
determinism rules, common causes of `ErrNonDeterminism`, and an
end-to-end crash-then-replay example.

## Providers

### OpenAI-compatible

The `openai` package is the OpenAI-compatible provider. Point it at
any compatible API with `WithBaseURL`:

```go
prov, _ := openai.New(
	openai.WithAPIKey(os.Getenv("GROQ_API_KEY")),
	openai.WithBaseURL("https://api.groq.com/openai/v1"),
)
// then use model "llama-3.1-8b-instant" etc.
```

Same pattern works for Together, OpenRouter, Ollama, vLLM, LM Studio,
Azure OpenAI.

### Anthropic

The `anthropic` package speaks the Messages API directly, including
extended-thinking with signature replay, redacted-thinking blocks,
and per-message prompt caching via `Message.Annotations`:

```go
import "github.com/jerkeyray/starling/provider/anthropic"

prov, _ := anthropic.New(anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
// Model: "claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5", ...
```

See [`docs/PROVIDER_SUPPORT.md`](./docs/PROVIDER_SUPPORT.md) for the
full feature matrix across both providers.

## Budgets

Set any combination of the four budget axes on `Agent.Budget`. Zero
values disable that axis. When a cap trips the runtime emits a
`BudgetExceeded` event and terminates with
`RunFailed{ErrorType:"budget"}`:

```go
a := &starling.Agent{
	Provider: prov,
	Tools:    tools,
	Log:      log,
	Budget: &starling.Budget{
		MaxInputTokens:  100_000,            // pre-call, before every LLM call
		MaxOutputTokens: 4_000,              // mid-stream, on every usage chunk
		MaxUSD:          0.50,               // mid-stream, using per-model prices
		MaxWallClock:    30 * time.Second,   // context.WithDeadline on the run
	},
	Config: starling.Config{Model: "gpt-4o-mini", MaxTurns: 8},
}
```

## Retry transient tool errors

Tools that know a failure is retryable (HTTP 503, rate-limit,
transient network) wrap the error with `tool.ErrTransient`:

```go
return nil, fmt.Errorf("upstream 503: %w", tool.ErrTransient)
```

Callers opt into retry per call, and only for operations they're
comfortable re-executing:

```go
step.CallTool(ctx, step.ToolCall{
	Name:        "fetch",
	Args:        args,
	Idempotent:  true,
	MaxAttempts: 3,
	// Backoff is optional — default is exponential 100ms → 10s
	// with 0–25% jitter. In replay, sleeps are skipped.
})
```

Non-idempotent calls (or calls without `MaxAttempts > 1`) run exactly
once, regardless of transience.

## What's in the box

Agent runtime:
- `Agent` + ReAct loop with `MaxTurns` cap
- Typed tools via `tool.Typed[In, Out]`
- Parallel tool dispatch (`step.CallTools`)
- Opt-in retry with exponential backoff for idempotent tools

Providers:
- OpenAI-compatible streaming provider (OpenAI, Groq, Together,
  OpenRouter, Ollama, vLLM, LM Studio, Azure)
- Anthropic Messages provider with extended-thinking + caching

Event log:
- In-memory backend (`eventlog.NewInMemory`)
- SQLite backend (`eventlog.NewSQLite`) with WAL-mode durability
- BLAKE3 hash chain + Merkle root on every terminal event
- `eventlog.Validate` for full-run tamper detection

Replay & budgets:
- `starling.Replay` verifier with `ErrNonDeterminism`
- Deterministic helpers: `step.Now`, `step.Random`, `step.SideEffect`
- All four budget axes: input tokens, output tokens, USD, wall-clock
- Per-model USD cost lookup

Observability & multi-tenant:
- `Config.Logger *slog.Logger` for structured side-channel trace
- OpenTelemetry spans (`agent.run` → `agent.turn` → `agent.llm_call`
  / `agent.tool_call`); no-op when no SDK is wired
- `Agent.Namespace` prefixes RunIDs so one event log can host many
  tenants safely

## Observability

Three layers, mix and match: the **event log** (audit trail),
**`log/slog`** (live trace via `Config.Logger`), and **OpenTelemetry**
spans around every step boundary (no-op tracer when no SDK is
configured). See
[`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) §6.3 for the full
picture and §6.2 for the synchronous-write / backpressure contract.

## Docs

- [`docs/API.md`](./docs/API.md) — public API reference
- [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) — package layout and data flow
- [`docs/DECISIONS.md`](./docs/DECISIONS.md) — design decisions (ADR-style)
- [`docs/EVENTS.md`](./docs/EVENTS.md) — event schema + CBOR wire format
- [`docs/REPLAY.md`](./docs/REPLAY.md) — replay cookbook
- [`docs/PROVIDER_SUPPORT.md`](./docs/PROVIDER_SUPPORT.md) — provider feature matrix
- [`docs/M2_PLAN.md`](./docs/M2_PLAN.md) — M2 scope + status

## License

Apache 2.0 — see [LICENSE](LICENSE).
