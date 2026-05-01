# Getting started

Starling is a Go runtime where every agent run is recorded as an
append-only, BLAKE3-chained event log. This page gets you from "I have
a Go module" to a working agent in about ten minutes.

## Install

```bash
go get github.com/jerkeyray/starling@v0.1.0-beta.1
```

Pin a tag — Starling is in beta and the API is allowed to change
between cuts. See [CHANGELOG.md](../CHANGELOG.md) for what each tag
ships.

You'll also need an API key for one of the supported providers. The
shortest path is OpenAI:

```bash
export OPENAI_API_KEY=sk-...
```

## Your first agent

The full source for the snippet below is in
[`examples/hello/main.go`](../examples/hello/main.go) — about 50 lines
end-to-end. Run it with `go run ./examples/hello`.

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    starling "github.com/jerkeyray/starling"
    "github.com/jerkeyray/starling/eventlog"
    "github.com/jerkeyray/starling/provider/openai"
)

func main() {
    prov, err := openai.New(openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
    if err != nil { log.Fatal(err) }

    log_ := eventlog.NewInMemory()
    defer log_.Close()

    agent := &starling.Agent{
        Provider: prov,
        Log:      log_,
        Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
    }
    res, err := agent.Run(context.Background(),
        "Give me a three-bullet summary of event-sourced agents.")
    if err != nil { log.Fatal(err) }
    fmt.Println(res.FinalText)
}
```

What this demonstrates:

- **Provider** is the LLM backend. Swap in `anthropic.New(...)`,
  `gemini.New(...)`, `bedrock.New(...)`, or any OpenAI-compatible
  endpoint via `openai.WithBaseURL(...)`.
- **Log** is the event log. `eventlog.NewInMemory()` is fine for
  scripts and tests; for anything you want to keep across restarts,
  use [`eventlog.NewSQLite(...)`](#durable-storage-and-the-inspector)
  and the inspector picks it up.
- **Agent.Run** drives the loop end-to-end and returns a
  [`RunResult`](../result.go) with totals, the recorded run's Merkle
  root, and the terminal kind.

## Adding a tool

Tools are pure Go functions, exposed to the model via JSON Schema:

```go
import "github.com/jerkeyray/starling/tool"

type echoIn  struct { Msg string `json:"msg"` }
type echoOut struct { Got string `json:"got"` }

echo := tool.Typed("echo", "echo the message back",
    func(_ context.Context, in echoIn) (echoOut, error) {
        return echoOut{Got: in.Msg}, nil
    })

agent.Tools = []tool.Tool{echo}
```

Tool calls show up as their own events in the log, are retryable, and
are deterministically replayable — see
[mental-model.md](mental-model.md) for the lifecycle.

## Durable storage and the inspector

Switch the log to SQLite and you can browse runs with the bundled
inspector:

```go
log_, err := eventlog.NewSQLite("runs.db")
```

Then, in another shell:

```bash
go run github.com/jerkeyray/starling/cmd/starling-inspect runs.db
```

The browser opens to a list of every run, per-run totals (cost,
tokens, duration), and a per-event timeline. There's also a
[`/diff`](#) page that shows two runs side-by-side aligned by
sequence number — useful for narrowing where two near-identical runs
diverged.

## Replay

Every recorded run can be replayed against the same agent wiring:

```go
err := starling.Replay(ctx, log_, runID, agent)
```

A clean replay returns nil. If today's behavior diverges from what
was recorded — different tool output, different model output, a new
event in the middle — you get `errors.Is(err,
starling.ErrNonDeterminism)` and the inspector's replay view shows
exactly which event mismatched.

If the recording came from a different provider or model, Replay
fails fast with `starling.ErrProviderModelMismatch` before any work
runs. Pass `starling.WithForceProvider()` to bypass when comparing
providers is the point.

## Where to next

- **[mental-model.md](mental-model.md)** — what a Run actually is,
  when it terminates, when to use one Run versus many.
- **[examples/m1_hello](../examples/m1_hello)** — a fuller dual-mode
  binary that wraps run / inspect / replay / reset / show into a
  single CLI.
- **[examples/incident_triage](../examples/incident_triage)** — a
  production-shaped workflow with budgets, MCP tools, OpenTelemetry,
  and durable Postgres storage.
