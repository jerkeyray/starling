# Reference: tools

The `tool.Tool` contract, the typed `tool.Typed` helper, the
`tool.Wrap` middleware composition, and the runtime's retry /
panic / replay behavior. Source of truth:
[`tool/tool.go`](../../tool/tool.go),
[`tool/typed.go`](../../tool/typed.go),
[`tool/wrap.go`](../../tool/wrap.go),
[`step/tools.go`](../../step/tools.go).

## The interface

```go
type Tool interface {
    Name() string
    Description() string
    Schema() json.RawMessage
    Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}
```

- **Name** must be stable (it identifies the tool to the model and
  in the event log).
- **Description** is what the model reads to decide when to call.
- **Schema** is JSON Schema. The bytes must be deterministic across
  calls - `RunStarted.ToolRegistryHash` is computed over them.
- **Execute** is invoked by the agent loop, MCP wrappers, or your
  own code via `step.CallTool`.

Implementations are expected to be safe for concurrent use by
multiple goroutines.

## Typed tools (`tool.Typed`)

`tool.Typed[In, Out]` wraps a strongly-typed function as a Tool.
The schema is derived from `In` via reflection at construction
time.

```go
type echoIn  struct { Msg string `json:"msg"` }
type echoOut struct { Got string `json:"got"` }

echo := tool.Typed("echo", "echo the message back",
    func(_ context.Context, in echoIn) (echoOut, error) {
        return echoOut{Got: in.Msg}, nil
    })
```

Constraints on `In`:

- **Must be a struct.** LLM tool schemas are JSON objects at the
  top level; non-struct types panic at construction with a
  message that suggests wrapping.
- Field types must be JSON Schema-derivable: `string`, numeric
  types, `bool`, slices/arrays of those, and structs of those.
  Maps, interfaces, recursive structs, and channels are not
  supported and panic the schema generator.

`Out` is encoded with `encoding/json` for the `Result` returned to
the agent. Fields tagged `json:"-"` are dropped, matching the
standard library.

## Errors and retries

Two sentinels in [`tool/tool.go`](../../tool/tool.go):

- `tool.ErrPanicked` - the tool function panicked. `tool.Typed`
  recovers internally and wraps the panic; `step.CallTool` recovers
  for raw `Tool` implementations too. Surfaced on
  `ToolCallFailed.ErrorType == "panic"`.
- `tool.ErrTransient` - the tool's failure is likely to succeed on
  retry. Wrap with `fmt.Errorf("upstream 503: %w",
  tool.ErrTransient)`.

`step.CallTool` retry policy:

| `ToolCall.Idempotent` | `ToolCall.MaxAttempts` | Behavior |
| --- | --- | --- |
| `false` | any | Single attempt regardless. Errors propagate. |
| `true` | `<= 1` | Single attempt. |
| `true` | `> 1` | Retries on `errors.Is(err, tool.ErrTransient)`. Non-transient errors break the retry loop. `ctx` cancellation always breaks. |

Each attempt emits a fresh `ToolCallScheduled` with `Attempt`
incremented (1-based). The final `ToolCallFailed.Final == true`
when retries are exhausted.

`ctx` cancellation surfaces as
`ToolCallFailed.ErrorType == "cancelled"`.

## Composing middleware (`tool.Wrap`)

`tool.Wrap` layers `Middleware` around `Execute` without
re-implementing the `Tool` interface:

```go
type Middleware func(
    inner func(context.Context, json.RawMessage) (json.RawMessage, error),
) func(context.Context, json.RawMessage) (json.RawMessage, error)
```

Composition is outer-first (matches `net/http.Handler` chaining):

```go
withAuth := func(inner ...) ... { /* check auth, then call inner */ }
withTime := func(inner ...) ... { /* time, then call inner */ }
wrapped := tool.Wrap(myTool, withAuth, withTime)
// On Execute: withAuth runs → calls withTime → calls myTool.Execute
```

A middleware that returns without calling `inner` short-circuits
the chain - useful for auth gates or input validation.

`Wrap` preserves `Name`, `Description`, and `Schema`, so the
schema hash recorded in `RunStarted.ToolRegistryHash` is identical
to the unwrapped tool. The wrap is invisible to the model and to
replay.

## Replay behavior

`ToolCallCompleted.Result` is the recorded JSON output. On replay
the runtime re-executes the live `Tool.Execute` and byte-compares
the returned bytes against the recording:

- **Match** → no event emitted (the chain is reproducing the
  recording byte-for-byte from the original `ToolCallCompleted`).
- **Mismatch** → replay surfaces a `Divergence` with `Class ==
  "payload"` and the run aborts.

If your tool's output depends on time, randomness, or external
state, the divergence is real signal: use
[`step.SideEffect`](step-primitives.md) inside the tool to record
the nondeterminism.

`SideEffectRecorded` events emitted from inside a tool are part of
the same byte-compared chain - they have to match the recording.

## MCP tools

Remote MCP tools go through
[`tool/mcp`](../../tool/mcp/client.go), which wraps the MCP
protocol's tool-call envelope into the `tool.Tool` interface. Each
remote call is automatically wrapped in `step.SideEffect` so
replay returns the recorded result without re-contacting the MCP
server.

Currently supported: stdio subprocess (`toolmcp.NewCommand`),
streamable HTTP (`toolmcp.NewHTTP`), custom transports
(`toolmcp.New`). Resources, prompts, and sampling are out of scope.

Per-call output is capped by `WithMaxOutputBytes` to keep run
sizes bounded.

## Built-in tools

`tool/builtin` ships small stock tools for common examples.
`builtin.Fetch()` performs a GET against public `http`/`https`
URLs only: it rejects localhost, private networks, link-local
addresses, multicast, unspecified addresses, and redirects that
land on those targets. It is still a compact fetch primitive, not
a browser or crawler; production agents should wrap or replace it
when they need allowlists, auth, custom headers, content-type
policy, or stricter retention controls.

## See also

- [reference/events.md](events.md) - `ToolCallScheduled`,
  `ToolCallCompleted`, `ToolCallFailed` payload tables.
- [reference/replay.md](replay.md) - how recorded tool calls
  are compared during replay.
- [`tool/builtin`](../../tool/builtin) - small set of stock tools
  shipped with the runtime.
