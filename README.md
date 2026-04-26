<div align="center">

# Starling

**Event-sourced agent runtime for Go**

[![CI](https://github.com/jerkeyray/starling/actions/workflows/ci.yml/badge.svg)](https://github.com/jerkeyray/starling/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/jerkeyray/starling.svg)](https://pkg.go.dev/github.com/jerkeyray/starling)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Replayable runs · Tamper-evident logs · Provider-neutral tools · Production debugging

</div>

> Status: pre-release. Starling is production-oriented, but the public API may
> change before v1. Requires Go 1.26+.

Starling is a Go runtime for building LLM agents where every run is recorded as
an append-only, BLAKE3-chained, Merkle-rooted event log. When an agent fails in
production, you can inspect the log, replay it against the same agent wiring, and
see exactly where today's behavior diverges from the original recording.

## Why Starling?

| If you need... | Starling gives you... |
| --- | --- |
| Debuggable agent runs | A complete event stream for prompts, tool calls, provider chunks, usage, budgets, and errors. |
| Portable replay | Deterministic re-execution from the recorded log, including MCP tool side effects. |
| Audit evidence | Hash-chained events and Merkle roots suitable for retention, review, and incident timelines. |
| Cost control | Token, USD, and wall-clock budgets enforced inside the runtime, not just observed after the fact. |
| Provider choice | OpenAI-compatible, Anthropic, Gemini, OpenRouter, and local/open model backends. |
| Operational visibility | Metrics hooks, structured divergence logs, and an embedded inspector UI. |

## Features

- **Event-sourced execution**: every meaningful runtime action is an event.
- **Deterministic replay**: recorded runs can be replayed without calling the
  model or re-running recorded side effects.
- **Durable event logs**: in-memory, SQLite, and Postgres backends with schema
  migration and validation helpers.
- **Provider adapters**: OpenAI-compatible APIs, Anthropic, Gemini, and
  OpenRouter.
- **MCP tools**: stdio subprocess and streamable HTTP clients backed by the
  official Go MCP SDK.
- **Tool safety**: retries, transient error classification, typed tool errors,
  max MCP output caps, and replay-safe side effects.
- **Inspector**: dependency-free browser UI for exploring runs and replay
  divergence.
- **Observability**: metrics wrappers, OpenTelemetry-friendly examples, and
  structured `slog` output.

## Install

```bash
go get github.com/jerkeyray/starling
```

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"os"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider/openai"
)

func main() {
	ctx := context.Background()

	prov, err := openai.New(openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	if err != nil {
		panic(err)
	}

	log := eventlog.NewInMemory()
	a := &starling.Agent{
		Provider: prov,
		Log:      log,
		Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 4},
	}

	run, err := a.Run(ctx, "Give me a three bullet incident summary.")
	if err != nil {
		panic(err)
	}

	fmt.Println(run.FinalText)
}
```

## Core Model

```text
Agent.Run
  -> provider.Stream
  -> tool execution
  -> budget checks
  -> append-only event log
  -> replay / inspect / resume
```

Starling treats the event log as the source of truth. The runtime records model
requests, streaming chunks, tool calls, usage, budget decisions, terminal states,
and replay metadata as structured events. Backends validate event ordering,
schema versions, and hash continuity.

## Durable Logs

Use SQLite or Postgres when runs must survive process restarts or be inspected
later.

```go
log, err := eventlog.NewSQLite("starling.db")
if err != nil {
	panic(err)
}
defer log.Close()
```

Durable backends support schema preflight checks, migrations, validation, and
retention workflows. See [docs/EVENTS.md](docs/EVENTS.md) and
[docs/RETENTION.md](docs/RETENTION.md).

## Replay And Resume

Replay a recorded run against the same agent wiring:

```go
if err := starling.Replay(ctx, log, runID, a); err != nil {
	if errors.Is(err, starling.ErrNonDeterminism) {
		// Inspect the log for the first diverging event.
	}
	panic(err)
}
```

Resume continues from a persisted run while preserving call correlation and
budget accounting.

```go
next, err := a.Resume(ctx, runID, "Continue with remediation steps.")
```

## Providers

| Provider | Package | Notes |
| --- | --- | --- |
| OpenAI-compatible | `provider/openai` | OpenAI, Groq, Together, Ollama, vLLM, LM Studio, Azure OpenAI, and compatible APIs via custom `BaseURL`. |
| Anthropic | `provider/anthropic` | Messages API support, tool use, thinking/signatures, and prompt caching metadata. |
| Gemini | `provider/gemini` | Native Gemini adapter for Google models. |
| OpenRouter | `provider/openrouter` | OpenRouter-specific convenience wrapper over the OpenAI-compatible path. |

Provider behavior is covered by a conformance suite so adapters share the same
streaming, usage, tool-call, and error contracts. See
[docs/PROVIDER_SUPPORT.md](docs/PROVIDER_SUPPORT.md).

## MCP Tools

Starling can expose remote MCP tools as regular `tool.Tool` values.

```go
client, err := toolmcp.NewCommand(ctx,
	exec.Command("uvx", "mcp-server-filesystem", "/tmp"),
	toolmcp.WithIncludeTools("read_file", "list_directory"),
	toolmcp.WithMaxOutputBytes(64<<10),
)
if err != nil {
	panic(err)
}
defer client.Close()

tools, err := client.Tools(ctx)
if err != nil {
	panic(err)
}

a := &starling.Agent{
	Provider: prov,
	Log:      log,
	Tools:    tools,
	Config:   starling.Config{Model: "gpt-4o-mini", MaxTurns: 8},
}
```

Supported transports:

- `toolmcp.NewCommand(ctx, cmd, opts...)` for stdio subprocess servers.
- `toolmcp.NewHTTP(ctx, endpoint, httpClient, opts...)` for streamable HTTP servers.
- `toolmcp.New(ctx, transport, opts...)` for custom transports.

MCP tool calls are wrapped in `step.SideEffect`, so replay uses the recorded
result instead of contacting the remote MCP server again. Starling currently
supports MCP tools; resources, prompts, and sampling are intentionally deferred.

## Budgets And Retries

Budgets can cap input tokens, output tokens, USD cost, and wall-clock runtime.

```go
a := &starling.Agent{
	Provider: prov,
	Log:      log,
	Budget: &starling.Budget{
		MaxInputTokens:  20_000,
		MaxOutputTokens: 4_000,
		MaxUSD:          0.50,
		MaxWallClock:    30 * time.Second,
	},
	Config: starling.Config{Model: "gpt-4o-mini", MaxTurns: 8},
}
```

Tool retries are explicit and replay-aware:

```go
out, err := step.CallTool(ctx, step.ToolCall{
	CallID:      "fetch-ticket",
	TurnID:      turnID,
	Name:        "fetch_ticket",
	Args:        args,
	Idempotent:  true,
	MaxAttempts: 3,
})
```

## Inspector

The inspector serves a local browser UI for recorded runs.

```bash
go run ./cmd/starling-inspect starling.db
```

Open `http://localhost:8080` to inspect event streams, tool calls, usage,
budgets, replay results, and divergence details. The inspector is a self-hosted
Go server with no CDN or JavaScript build step.

## Production Checklist

- Run `make check` before release: format, vet, build, race tests, lint, and
  vulnerability scan.
- Pick a durable log backend for production runs: SQLite for single-node use,
  Postgres for shared infrastructure.
- Run eventlog preflight and migrations during deploys.
- Protect inspector access behind your normal internal auth boundary.
- Set explicit budgets for tokens, cost, and wall-clock runtime.
- Use idempotent retries and per-call timeouts for tools that touch external
  systems.
- Use replay regression tests for critical agent workflows.
- Store raw provider responses only when your privacy and retention policy
  allows it.
- Review [docs/SECURITY.md](docs/SECURITY.md) and
  [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) before production use.

## Examples

| Example | What it shows |
| --- | --- |
| [examples/m1_hello](examples/m1_hello) | Minimal agent run. |
| [examples/incident_triage](examples/incident_triage) | End-to-end production-style workflow with budgets, replay, resume, metrics, OTel, and durable logs. |
| [examples/mcp_tools](examples/mcp_tools) | MCP server tools adapted into Starling tools. |
| [examples/m4_inspector_demo](examples/m4_inspector_demo) | Local run data for the inspector. |

## Documentation

| Document | Purpose |
| --- | --- |
| [docs/API.md](docs/API.md) | Public package layout and core APIs. |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Runtime architecture and data flow. |
| [docs/EVENTS.md](docs/EVENTS.md) | Event schema, validation rules, and migration notes. |
| [docs/REPLAY.md](docs/REPLAY.md) | Replay semantics and determinism rules. |
| [docs/REPLAY_DEBUGGING.md](docs/REPLAY_DEBUGGING.md) | Debugging failed runs with replay and inspector workflows. |
| [docs/PROVIDER_SUPPORT.md](docs/PROVIDER_SUPPORT.md) | Provider feature matrix and adapter notes. |
| [docs/INSPECT.md](docs/INSPECT.md) | Inspector usage, flags, and security model. |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | Runtime deployment guidance. |
| [docs/SECURITY.md](docs/SECURITY.md) | Security model and operator responsibilities. |
| [docs/RETENTION.md](docs/RETENTION.md) | Event retention and privacy guidance. |
| [docs/DECISIONS.md](docs/DECISIONS.md) | Design decisions and tradeoffs. |
| [docs/PERF_TUNING.md](docs/PERF_TUNING.md) | Performance tuning notes and benchmarks. |

## Development

```bash
make check
```

Useful targets:

```bash
make test      # race-enabled Go test suite
make lint      # golangci-lint
make vuln      # govulncheck
make inspect   # run the inspector locally
make smoke     # quick end-to-end smoke run
```

## License

Apache 2.0. See [LICENSE](LICENSE).
