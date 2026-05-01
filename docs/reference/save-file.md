# Reference: anatomy of a save file

What's actually inside `runs.db`. SQLite tables, what each column
holds, how CBOR payloads round-trip, and the WAL sidecars that bite
when you copy the file naïvely. Source of truth:
[`eventlog/sqlite.go`](../../eventlog/sqlite.go).

## Files on disk

A SQLite-backed event log opened in WAL mode (the default) lives
across **three** files:

| File | Purpose |
| --- | --- |
| `runs.db` | Main database file. Contains the schema, tables, and historical pages. |
| `runs.db-wal` | Write-ahead log. Holds recently committed pages until checkpoint. |
| `runs.db-shm` | Shared memory file. Coordinates concurrent readers and the writer. |

A naïve `cp runs.db backup.db` is **not safe** while the database
is open: WAL pages aren't yet in the main file, so the backup is
missing recent commits. Use `eventlog.ForkSQLite` (which calls
SQLite's `VACUUM INTO`) to copy logs safely. See
[cookbook/branching.md](../cookbook/branching.md).

## Schema

Two tables, both prefixed `eventlog_` so they don't collide with
host-app tables.

### `eventlog_events`

The append-only event ledger. One row per event.

```sql
CREATE TABLE eventlog_events (
    run_id    TEXT    NOT NULL,
    seq       INTEGER NOT NULL,
    prev_hash BLOB,
    ts        INTEGER NOT NULL,    -- nanoseconds since Unix epoch
    kind      INTEGER NOT NULL,    -- event.Kind, uint8 widened to int
    payload   BLOB    NOT NULL,    -- canonical CBOR bytes
    PRIMARY KEY (run_id, seq)
);
```

Field-by-field:

- `run_id` — the agent's `RunID`. May be `Namespace + "/" + ULID`
  if `Config.Namespace` is set.
- `seq` — 1-based monotonic per-run counter. Combined with `run_id`
  forms the primary key; this index covers every read pattern the
  runtime uses (read-by-run, list-runs, stream-tail).
- `prev_hash` — BLAKE3-32 of the canonical CBOR encoding of the
  prior event. NULL on the first event of a run; required on
  every subsequent event.
- `ts` — wall-clock timestamp at append time, in nanoseconds since
  Unix epoch. Replay reuses recorded timestamps so chain hashes
  align.
- `kind` — `event.Kind` value (uint8). Stored as INTEGER for SQLite
  ergonomics.
- `payload` — canonical CBOR bytes of the per-kind payload struct.
  Encoded via `cborenc.Marshal` (RFC 8949 §4.2 deterministic).

### `eventlog_schema_migrations`

The on-disk schema version pin. One row per applied migration.

```sql
CREATE TABLE eventlog_schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL    -- nanoseconds since epoch
);
```

`SELECT MAX(version)` is the current on-disk schema. The `starling
schema-version <db>` CLI prints it; `starling migrate <db>` runs
forward migrations to bring the file in line with the binary's
expected version.

## Pragmas applied at open

`NewSQLite` runs the following on every fresh open (read-only opens
skip the migration step):

| Pragma | Value | Why |
| --- | --- | --- |
| `journal_mode` | `WAL` | Concurrent reads alongside one writer. |
| `synchronous` | `NORMAL` | Trades a tiny crash-window for ~3× write throughput. |
| `foreign_keys` | `ON` | Belt-and-braces; current schema has no FKs but future migrations may. |
| `busy_timeout` (DSN) | 5000 ms | Per-connection; wait up to 5 s on lock contention before erroring. |
| `_txlock` (DSN) | `immediate` | `BeginTx` grabs the write lock upfront — closes the read-then-insert window. |

Read-only opens use `mode=ro` in the DSN. **Not** `immutable=1` —
the inspector is expected to be safe to point at a database that
another Starling process is actively writing to, and `immutable=1`
would let SQLite skip change-counter checks and serve stale reads.

## CBOR payload layout

Every `payload` blob is the canonical CBOR encoding of one of the
typed payload structs in
[`event/types.go`](../../event/types.go) — see
[reference/events.md](events.md) for the per-kind field tables.

The encoding is deterministic per RFC 8949 §4.2:

- shortest-form integer encoding;
- shortest-form floating-point, NaN normalised to `0x7e00`;
- map keys sorted bytewise lexical on their CBOR-encoded form;
- definite-length encoding only.

This determinism is what makes `event.Hash(event.Marshal(ev))`
stable: byte-identical inputs hash byte-identically across runs,
processes, and machines. The chain only works because of this.

The decoder is strict: duplicate map keys
(`DupMapKeyEnforcedAPF`) and indefinite-length items
(`IndefLengthForbidden`) are rejected. A corrupted-or-tampered
payload that decoded successfully would invalidate the chain
without surfacing as a parse error; refusing those constructions
keeps `eventlog.Validate` honest.

## Inspecting raw bytes

For debugging:

```bash
# every event's run_id, seq, kind, hex payload
sqlite3 runs.db "
  SELECT run_id, seq, kind, hex(payload)
  FROM eventlog_events
  ORDER BY run_id, seq
  LIMIT 20;
"
```

To turn a payload back into JSON, the easiest path is the
inspector (`starling-inspect runs.db`), which calls
`event.ToJSON` per row. For programmatic access:

```go
evs, _ := log.Read(ctx, runID)
for _, ev := range evs {
    raw, _ := event.ToJSON(ev)
    fmt.Println(string(raw))
}
```

`starling export <db> <runID>` does the same, NDJSON-style, for
piping into `jq`.

## Postgres backend

`eventlog.NewPostgres` uses the same logical schema with
PostgreSQL-flavored types (`BYTEA`, `BIGINT`, primary key on
`(run_id, seq)`) and the same migrations table. The big difference
is no WAL sidecar files — `pg_dump`/`pg_basebackup` are the
canonical safe-copy tools, and a counterpart to
`eventlog.ForkSQLite` for Postgres is on the roadmap.

## See also

- [reference/events.md](events.md) — the typed payloads stored
  in the `payload` column.
- [cookbook/branching.md](../cookbook/branching.md) — WAL-safe
  forking via `eventlog.ForkSQLite`.
- [`eventlog/sqlite.go`](../../eventlog/sqlite.go) — schema,
  migrations, query plan.
