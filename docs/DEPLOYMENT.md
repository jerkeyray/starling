# Deployment

How to run Starling in production. Read [SECURITY.md](SECURITY.md) and
[RETENTION.md](RETENTION.md) alongside this — they cover decisions that
deployment alone cannot enforce.

## Process model

Starling is a Go library. There is no Starling server. Two common
shapes:

1. **Embedded.** The agent runs inside your existing Go service. The
   event log is a file (SQLite) or a connection (Postgres) the service
   already manages. The inspector ships as a separate binary or a
   subcommand of your binary, pointed at the same DB read-only.
2. **Sidecar inspector.** The agent runs in service A; the inspector
   runs in service B with a read-only connection to the same DB. Use
   this when operators need debugging access without redeploying the
   primary service.

There is no required scheduler. `Agent.Run` is a blocking Go call that
returns when the run terminates.

## Picking a backend

| Backend | Use when | Avoid when |
|---|---|---|
| `eventlog.NewInMemory` | Tests, ephemeral tools. | Anything you want to replay later. |
| `eventlog.NewSQLite` | Single-host services, dev environments, edge nodes. WAL mode + per-run `_txlock=immediate` makes one-writer-many-readers correct. | Multi-host writers — SQLite has no cross-host locking. |
| `eventlog.NewPostgres` | Multi-host services, regulated workloads, anything that needs PITR / replication. Per-run `pg_advisory_xact_lock` serializes appenders by run. | Workloads where the DB is unavailable for long stretches; the agent has no offline buffer. |

## Schema migrations

Every `eventlog.NewSQLite` open auto-migrates to the latest schema.
Postgres callers must run migrations explicitly:

```bash
# CLI (SQLite)
starling migrate /var/lib/starling/log.db
starling schema-version /var/lib/starling/log.db
```

```go
// In-process (any backend)
log, err := eventlog.NewPostgres(db)
if err != nil { return err }
if _, err := eventlog.Migrate(ctx, log); err != nil { return err }
```

`Agent.Run` / `Agent.Resume` / inspector startup runs `eventlog.Preflight`
and refuses to operate against a stale or too-new schema. Disable with
`Config.SkipSchemaCheck = true` only in tests.

## SQLite

```go
log, err := eventlog.NewSQLite("/var/lib/starling/log.db")
```

- WAL mode is on (`PRAGMA journal_mode=WAL`); fsync on commit is
  `synchronous=NORMAL`. Set `=FULL` if you need stronger guarantees and
  can pay the latency.
- File permissions: chmod `0600`, owned by the agent user.
- One process per file. Multiple processes can read concurrently
  (`WithReadOnly`), but only one process should ever write — use
  Postgres for multi-writer.
- Backup: `sqlite3 log.db ".backup /tmp/log-backup.db"` while the
  agent is running. Restore by stopping the agent, swapping the file,
  and restarting. `sqlite3 .recover` for damaged files.

## Postgres

```go
db, _ := sql.Open("postgres", os.Getenv("DATABASE_URL"))
db.SetMaxOpenConns(8)
log, err := eventlog.NewPostgres(db, eventlog.WithAutoMigratePG())
```

- Postgres ≥ 11 (uses `hashtextextended` for advisory locks).
- Connection pool: size to expected concurrent runs + a few headroom
  connections for the inspector.
- Per-run advisory locks (`pg_advisory_xact_lock`) serialize appends
  for a given `run_id`. Different runs are independent.
- Backup: standard `pg_dump --table=eventlog_events` for logical
  exports; PITR via WAL archiving for hot recovery.
- Restore: `pg_restore` into an empty schema, then run
  `eventlog.Migrate`.

## Docker example

```dockerfile
FROM golang:1.23 AS build
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agent ./cmd/your-agent

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/agent /agent
USER 65532:65532
ENTRYPOINT ["/agent"]
```

Mount the SQLite file from a persistent volume, or set `DATABASE_URL`
to a managed Postgres.

## Kubernetes sketch

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: starling-agent }
spec:
  replicas: 1                        # SQLite: one writer
  selector: { matchLabels: { app: starling-agent } }
  template:
    metadata: { labels: { app: starling-agent } }
    spec:
      containers:
        - name: agent
          image: ghcr.io/your-org/agent:0.1.0
          env:
            - name: STARLING_INSPECT_TOKEN
              valueFrom: { secretKeyRef: { name: starling, key: token } }
            - name: ANTHROPIC_API_KEY
              valueFrom: { secretKeyRef: { name: starling, key: anthropic } }
          volumeMounts:
            - name: log
              mountPath: /var/lib/starling
      volumes:
        - name: log
          persistentVolumeClaim: { claimName: starling-log }
```

For Postgres, drop the volume + replica cap and use a managed DB.

## Inspector

```bash
# Front with TLS-terminating reverse proxy. See SECURITY.md.
STARLING_INSPECT_TOKEN=$(openssl rand -hex 32) \
  starling inspect -addr :8080 /var/lib/starling/log.db
```

The stock binary is view-only. To enable replay re-execution, build a
dual-mode binary that calls `starling.InspectCommand(yourFactory)`.

## Metrics

Prometheus metrics are exposed via `Agent.Metrics`. Wire them into your
existing handler:

```go
metrics, _ := starling.NewMetrics()
agent := &starling.Agent{ /* … */ Metrics: metrics }
http.Handle("/metrics", metrics.Handler())
```

See [METRICS.md](METRICS.md) for the metric set.

## Tracing

OpenTelemetry spans are emitted under the `starling` instrumentation
name. Wire any OTLP exporter:

```go
exp, _ := otlptracegrpc.New(ctx)
provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
otel.SetTracerProvider(provider)
```

Expected span tree per run: `agent.run` → `agent.turn` → `provider.stream`,
plus `step.tool` per tool dispatch.

## Failure recovery

A crashed run leaves an open hash chain. Restart the same `runID` with
`Agent.Resume(ctx, runID, "")`:

- If the crash happened mid-turn before `AssistantMessageCompleted`,
  `Resume` reissues pending tool calls under fresh CallIDs.
- If the assistant turn completed but tools were pending, same path.
- Pass `WithReissueTools(false)` to refuse and inspect manually.

`Agent.Resume` and `RunReplay` go through the same `eventlog.Preflight`
check as `Run`, so a stale schema fails fast.

## Operational checklist

- [ ] Backups verified by restoring into a staging DB monthly.
- [ ] Inspector behind TLS + bearer token.
- [ ] Provider API keys in env, not source.
- [ ] DB file/connection user has the minimum privileges.
- [ ] `eventlog.Migrate` in your release script (Postgres) or trusted
      to run on `NewSQLite`.
- [ ] Metrics scraped; dashboard alerts on `eventlog_append_seconds`
      p99 and `provider_call_seconds` p99.
- [ ] Retention policy implemented (see [RETENTION.md](RETENTION.md)).
- [ ] Security review for tool-side network/filesystem access.
