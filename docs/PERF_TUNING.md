# Performance tuning

Where the cycles go and the knobs that move them. Numbers in this
document are placeholders until W6 publishes reproducible benchmarks
under `bench/`.

## Where the time goes

A typical turn:

```
agent.run
├── eventlog.Append (RunStarted)
├── agent.turn
│   ├── provider.Stream  ← dominant: tens of ms to seconds, network-bound
│   │   ├── ChunkText × N
│   │   └── ChunkEnd
│   ├── eventlog.Append (TurnStarted)         ← microseconds
│   └── eventlog.Append (AssistantMessageCompleted)
└── step.tool × M (parallel)
    ├── tool.Execute        ← user code, can be anything
    ├── eventlog.Append (ToolCallScheduled)
    └── eventlog.Append (ToolCallCompleted)
```

Provider latency dominates. Append latency is microseconds for
in-memory, low-millisecond for SQLite/Postgres. Optimize tool code and
provider choice before tuning the event log.

## Append latency

| Backend | p50 (target) | p99 (target) |
|---|---|---|
| In-memory | <10 µs | <50 µs |
| SQLite (WAL, NORMAL) | <1 ms | <5 ms |
| Postgres (LAN) | <2 ms | <10 ms |

To measure in your environment, scrape
`starling_eventlog_append_seconds` (Prometheus histogram). High p99 with
low p50 usually points at SQLite checkpointing pressure or Postgres
connection saturation.

### SQLite knobs

- `synchronous=NORMAL` (default) is the right call for most workloads.
  `=FULL` adds an fsync per commit; expect 2–5× higher append latency.
- WAL checkpoints: under sustained write load, WAL can grow large and
  trigger long auto-checkpoints. Run `PRAGMA wal_autocheckpoint=1000;`
  (default) or trigger manual checkpoints between idle periods.
- `_busy_timeout=5000` (DSN) makes contended writers wait instead of
  returning `SQLITE_BUSY`. Tune up if you have many concurrent runs on
  one file.
- `MaxOpenConns` defaults to 8 — fine for one writer + a few readers.
  Raise it only if the inspector saturates the pool.

### Postgres knobs

- Connection pool: at least one writer connection per concurrent run.
  Reads (Read, ListRuns) come from the same pool.
- `pg_advisory_xact_lock` cost is negligible (single-digit microseconds);
  the cost shows up as serialization when many appenders fight over the
  same `run_id`. Different runs are independent.
- Statement timeouts in your DSN protect against runaway streams.
- Index footprint: only `(run_id, seq)` PK exists. Add covering indexes
  if you query by status/time outside `ListRuns` — see W7 (run search)
  for the planned index set.

## Stream buffering

In-memory streams use unbounded channels per subscriber. A slow
subscriber will block appends. The W5.2 work makes drops observable;
until then, treat the inspector or any external stream consumer as
"must keep up."

SQLite/Postgres stream loops poll every 50 ms (`sqliteStreamPollInterval`,
`pgStreamPollInterval`). Lower polling means lower latency at higher
idle query cost. Tune by patching the constants if necessary.

## Tool concurrency

`step.Config.MaxParallelTools` (default 8) caps how many tools a single
`CallTools` batch dispatches concurrently. Raise it for I/O-bound tools
(HTTP fetches, vector search), keep it low (1–2) for CPU-bound tools.
Per-tool timeouts live in your `tool.Tool.Execute` implementation; the
runtime does not enforce them.

## Provider concurrency

One agent run streams from one provider connection at a time. To
parallelize across runs, run multiple `Agent.Run` goroutines — each
with its own RunID. `provider.Provider` implementations must be
goroutine-safe.

## Replay throughput

`Agent.RunReplay` is bound by event-log read throughput plus tool
execution time replaced by recorded reads. Expect replay to be 10–100×
faster than the live run; the bottleneck is provider streaming
elimination.

For replay over very large logs, prefer `Agent.RunReplayInto` with a
caller-owned in-memory sink so allocations don't dominate.

## Memory per in-flight run

Per active `Agent.Run`:

- `step.Context` (small, fixed overhead).
- The replayed/live message slice (`provider.Message`s): grows linearly
  with conversation length.
- Tool args/results buffered until `ToolCallCompleted` is appended.

For long-running multi-turn agents, the message slice can reach MBs.
Cap with budget caps (`MaxOutputTokens`) and a context truncation
strategy (W9.5).

## Inspector replay

Replay through the inspector adds:

- An SSE subscriber per replay session.
- Server-side replay loop sharing the same `RunReplayInto` machinery
  as headless replay.

A modest server should handle dozens of concurrent replay sessions.
Watch CPU when tool args/results are large (the JSON projector runs
per-event for the timeline view).

## Benchmarks (TODO)

`bench/` will land in W6 with:

- Append p50/p99 across backends
- Replay throughput
- Memory per in-flight run
- Overhead vs raw provider call

Until then, scrape Prometheus and `pprof` your process:

```go
import _ "net/http/pprof"
go http.ListenAndServe("127.0.0.1:6060", nil)
```
