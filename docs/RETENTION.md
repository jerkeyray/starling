# Retention and archive

The event log is append-only by design. That is a feature for audit and
replay; it is a problem for storage growth. This document describes the
patterns operators use to keep growth bounded without breaking the
audit guarantees.

## What you cannot do

- **Mutate events.** Hash chain integrity depends on every committed
  event being immutable. Any change breaks `eventlog.Validate` for
  every run after that point.
- **Delete a single event from a run.** Same reason. The unit of
  deletion is the entire run (all events sharing a `run_id`).
- **Re-number runs.** `seq` is per-run and starts at 1; never reuse a
  retired `run_id`.

## What you can do

- **Delete whole runs.** Removing all events for a `run_id` is safe —
  no cross-run references.
- **Archive whole runs to cold storage.** Export to NDJSON via
  `starling export <db> <runID>` (or equivalent), store the file
  somewhere cheaper, then delete from the live DB.
- **Partition by time.** Postgres-only; see below.
- **Truncate the inspector view.** `RunSummary.StartedAt` lets you
  filter the inspector to recent runs without deleting old ones.

## Pattern: rolling deletion window

Simplest approach. Define a retention horizon (e.g. 90 days). Daily
job: delete every run whose terminal event is older than the horizon.

### SQLite

```sql
DELETE FROM eventlog_events
WHERE run_id IN (
    SELECT run_id FROM eventlog_events
    GROUP BY run_id
    HAVING MAX(ts) < (strftime('%s', 'now', '-90 days') * 1000000000)
);
VACUUM;
```

`VACUUM` reclaims pages; do it during low traffic. Note: SQLite holds
a write lock for the whole VACUUM.

### Postgres

```sql
DELETE FROM eventlog_events
WHERE run_id IN (
    SELECT run_id FROM eventlog_events
    GROUP BY run_id
    HAVING MAX(ts) < EXTRACT(EPOCH FROM NOW() - INTERVAL '90 days') * 1000000000
);
```

Pair with `VACUUM (ANALYZE) eventlog_events;` after large deletions.
For very large logs, batch the delete:

```sql
DELETE FROM eventlog_events
WHERE run_id IN (
    SELECT run_id FROM eventlog_events
    GROUP BY run_id
    HAVING MAX(ts) < EXTRACT(EPOCH FROM NOW() - INTERVAL '90 days') * 1000000000
    LIMIT 1000
);
```

…in a loop until the row count stops dropping.

## Pattern: archive then delete

For runs you want to keep indefinitely but out of the live DB:

```bash
# 1. Export each retained run to NDJSON.
for run in $(starling list-runs --before='90 days ago' /var/lib/starling/log.db); do
    starling export /var/lib/starling/log.db "$run" \
        | gzip > /archive/$run.ndjson.gz
done

# 2. Delete from live DB once archive is verified.
sqlite3 /var/lib/starling/log.db <<SQL
DELETE FROM eventlog_events WHERE run_id IN (...);
SQL
```

A run reconstructed from the archive can be replayed using the same
`Agent.RunReplay` API by reading the NDJSON back into a slice of
`event.Event`.

## Pattern: monthly partitions (Postgres)

For high-volume deployments, partition `eventlog_events` by month so
deletion becomes a `DROP PARTITION`. This is not built into the
schema; you'll need to fork the v1 migration or manage partitioning
externally.

Sketch:

```sql
CREATE TABLE eventlog_events (
    run_id    TEXT     NOT NULL,
    seq       BIGINT   NOT NULL,
    prev_hash BYTEA,
    ts        BIGINT   NOT NULL,
    kind      INTEGER  NOT NULL,
    payload   BYTEA    NOT NULL,
    PRIMARY KEY (run_id, seq, ts)         -- ts must be in the PK for partitioning
) PARTITION BY RANGE (ts);

CREATE TABLE eventlog_events_2026_04 PARTITION OF eventlog_events
    FOR VALUES FROM (ts_for('2026-04-01')) TO (ts_for('2026-05-01'));
```

`ts_for` is whatever you use to convert the boundary to nanoseconds.
Use `pg_partman` or a cron job to roll new partitions monthly. Drop
the oldest partition at the retention horizon.

This breaks Starling's PK assumption (`PRIMARY KEY (run_id, seq)`) —
test that the existing query patterns still hit the index efficiently
before adopting.

## PII deletion

When an end user requests deletion (GDPR / CCPA right to erasure):

1. Identify every `run_id` containing the user's data. Keep an external
   index (e.g. `user_id → []run_id`) — Starling does not maintain one.
2. `DELETE FROM eventlog_events WHERE run_id IN (...)`.
3. Also delete any archived NDJSON files.

Do not try to selectively redact within a run. The hash chain depends
on every event; rewriting one breaks every later event in the same run
*and* the validity of all replays.

If you anticipate frequent deletion, keep PII out of the log entirely:
redact at the tool boundary or store opaque IDs in tool args / results
and resolve them through a separate, mutable store.

## Backup interaction

Retention deletions and backups must agree on the horizon:

- Backup retention >= log retention. Otherwise restoring an old backup
  resurrects deleted PII.
- Document the lag: a 30-day backup horizon and 90-day log horizon
  means deleted runs reappear in restored snapshots for up to 30 days.

## Storage rule of thumb

Per `docs/EVENTS.md` §7, a 5-turn run with 10 tool calls is roughly
100–500 KB. Rough sizing:

- 1k runs/day × 200 KB ≈ 200 MB/day → 6 GB/month.
- Plan retention horizon around your DB's comfortable size, not your
  audit policy. If audit demands more, archive to NDJSON and retain
  only recent runs in the live DB.

## What does not exist yet

- Built-in `starling retention` command (planned).
- `starling dump` / `starling restore` commands (planned for W6).
- Automatic archive-on-delete hooks.

Until those land, retention is a cron job you write. The patterns above
are the production-tested shapes.
