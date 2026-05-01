# hello — your first Starling agent

The smallest end-to-end agent that runs against a real model. ~50 lines.

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello
```

Reads from `OPENAI_API_KEY`, runs one goal against `gpt-4o-mini`, prints
the final text. No replay, no inspect, no flags — once you're past
this, [examples/m1_hello](../m1_hello) shows the full dual-mode pattern
(run / inspect / replay / reset / show), and
[examples/incident_triage](../incident_triage) demonstrates a
production-shaped workflow with budgets, MCP tools, OpenTelemetry, and
durable storage.
