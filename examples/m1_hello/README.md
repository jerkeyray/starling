# m1_hello

Minimal end-to-end smoke demo for Starling's M1 milestone. Builds an
`Agent` with one tool (`current_time`), points it at an
OpenAI-compatible endpoint, runs one prompt, and prints the
`RunResult` plus a one-line dump of every event.

## Run against OpenAI

```sh
OPENAI_API_KEY=sk-... go run ./examples/m1_hello
```

Defaults to `gpt-4o-mini`. Override via `MODEL=...`.

## Run against Groq (same binary)

```sh
OPENAI_API_KEY=$GROQ_API_KEY \
OPENAI_BASE_URL=https://api.groq.com/openai/v1 \
MODEL=llama-3.1-8b-instant \
  go run ./examples/m1_hello
```

## What success looks like

```
=== RunResult ===
RunID:         01J...
FinalText:     The current UTC time is 2026-04-19T...
TerminalKind:  RunCompleted
TurnCount:     2
ToolCallCount: 1
...
=== Events ===
    1 RunStarted
    2 TurnStarted
    3 AssistantMessageCompleted
    4 ToolCallScheduled
    5 ToolCallCompleted
    6 TurnStarted
    7 AssistantMessageCompleted
    8 RunCompleted
  (8 total)
validate: ok
```

`validate: ok` confirms the Merkle-rooted hash chain produced by a
real run round-trips through `eventlog.Validate`.
