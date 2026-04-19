# Starling

**Status:** pre-alpha. M1 (MVP) shipped. Public API will move before v0.1.

Starling is an event-sourced agent runtime for Go. Every agent run is
recorded as an append-only, BLAKE3-chained, Merkle-rooted event log,
which means every execution is:

- **Replayable** — re-run a goal byte-for-byte from the log.
- **Auditable** — tamper with any prior event and the Merkle root in
  the terminal event no longer matches.
- **Cost-enforceable** — token and USD budgets enforced inline,
  emitted into the log.

## Install

```sh
go get github.com/jerkeyray/starling
```

Requires Go 1.22+.

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
	if err != nil { panic(err) }
	fmt.Println(res.FinalText)
}
```

A runnable version (with event-log dump and tamper-evidence check)
lives at [`examples/m1_hello`](./examples/m1_hello).

## OpenAI-compatible endpoints

The OpenAI provider is the OpenAI-compatible provider. Point it at any
compatible API with `WithBaseURL`:

```go
prov, _ := openai.New(
	openai.WithAPIKey(os.Getenv("GROQ_API_KEY")),
	openai.WithBaseURL("https://api.groq.com/openai/v1"),
)
// then use model "llama-3.1-8b-instant" etc.
```

Same pattern works for Together, OpenRouter, Ollama, vLLM, LM Studio,
Azure OpenAI.

## Anthropic

The Anthropic provider speaks the Messages API directly, including
extended-thinking with signature replay, redacted-thinking blocks,
and per-message prompt caching via `Message.Annotations`:

```go
import "github.com/jerkeyray/starling/provider/anthropic"

prov, _ := anthropic.New(anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
// Model: "claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5", ...
```

See [`docs/PROVIDER_SUPPORT.md`](./docs/PROVIDER_SUPPORT.md) for the
full feature matrix (what's supported, what's deferred) across both
providers.

## What's in M1

- `Agent` + ReAct loop with `MaxTurns` cap
- OpenAI-compatible streaming provider
- Typed tools via `tool.Typed[In, Out]`
- In-memory event log with hash-chain validation on append
- BLAKE3 Merkle root in every terminal event
- `eventlog.Validate` for full-run tamper detection
- Pre-call input-token budget enforcement
- Per-model USD cost lookup

## What's next (M2)

Replay verifier, SQLite log backend, parallel tool dispatch, Anthropic
provider, full budget axes (output tokens, USD, wall-clock).

## License

Apache 2.0 — see [LICENSE](LICENSE).
