# Examples

Runnable example agents will land here as milestones ship. Each example is
its own `main` package and builds independently with `go run ./examples/<name>`.

| Example | Description |
|---|---|
| [`m1_hello`](./m1_hello) | M1 smoke demo: one tool, one round-trip, against any OpenAI-compatible endpoint (or Anthropic with `PROVIDER=anthropic`). |

## Running

```sh
# OpenAI (default)
OPENAI_API_KEY=sk-... go run ./examples/m1_hello

# Anthropic
PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... go run ./examples/m1_hello

# Any OpenAI-compatible gateway
OPENAI_API_KEY=$GROQ_API_KEY \
OPENAI_BASE_URL=https://api.groq.com/openai/v1 \
MODEL=llama-3.1-8b-instant \
  go run ./examples/m1_hello
```
