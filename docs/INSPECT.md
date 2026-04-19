# `starling-inspect` — local web inspector

`starling-inspect` is a single Go binary that opens a Starling SQLite
event log read-only and serves a self-contained web UI on localhost
for browsing runs, walking the event timeline, inspecting payloads,
and seeing whether the hash chain validates.

It exists because once a run has more than ~50 events, the CLI dump
stops fitting in a terminal. The UI is intentionally pprof-shaped:
runs list → event timeline → detail pane → validation badge. No
auth, no telemetry, no CDN; HTMX is vendored, CSS is hand-rolled,
templates ship in the binary via `go:embed`.

## Install

```sh
go install github.com/jerkeyray/starling/cmd/starling-inspect@latest
```

Or from a checkout: `go build ./cmd/starling-inspect`.

The binary has no runtime dependencies beyond the SQLite file you
point it at.

## Demo (no API keys, no provider)

```sh
make demo-inspect
```

Seeds `/tmp/starling-demo.db` with four synthetic runs (one of each
terminal status plus an in-progress run, real Merkle roots so the
green ✓ badge is exercised) and launches the inspector against it.
Override the path with `make demo-inspect DB=/tmp/whatever.db`.

## Use against a real run

```sh
starling-inspect /path/to/runs.db
```

The default bind is `127.0.0.1:0` (a free port the kernel picks),
the URL is logged on startup, and your browser opens automatically.

## Flags

| Flag | Default | Notes |
|---|---|---|
| `<db>` (positional) | — | **Required.** Path to a Starling SQLite log. |
| `--addr=host:port` | `127.0.0.1:0` | Bind address. `:0` picks a free port. Wildcard hosts (`0.0.0.0`, `::`, `[::]`, empty) are normalized to `localhost` in the printed URL because no browser can open `[::]:43127`. |
| `--no-open` | `false` | Skip the browser launch. Use this over SSH or in a headless environment. |

```sh
# Headless / SSH
starling-inspect --no-open --addr=127.0.0.1:8080 runs.db

# Bind explicitly
make inspect DB=/path/to/runs.db
```

## Views

### Runs list (`/`)

Server-rendered table sorted newest-first. Columns: Run ID, Started,
Events, Status. Status is color-coded by terminal kind:

| Badge | Meaning |
|---|---|
| green `completed` | last event is `RunCompleted` |
| red `failed` | last event is `RunFailed` |
| yellow `cancelled` | last event is `RunCancelled` |
| gray `in progress` | no terminal event yet (run still going, crashed, or aborted) |

Status filter via `?status=completed | failed | cancelled | in+progress`
(top-right tabs). Client-side text filter (header search box) toggles
row visibility by run-id substring — purely local, no extra requests.

### Run detail (`/run/{runID}`)

Two panes:

- **Left**: scrollable event timeline. Each row shows seq, time,
  kind, and a one-line summary (`tool=fetch call=c1`,
  `model=gpt-4o-mini`, `call=c1 ok 18ms`, …). Rows are color-coded by
  family — lifecycle (blue), message (slate), tool (violet), budget
  (orange), terminal (red). Click → swaps the right pane via HTMX.
- **Right**: per-event detail. Header shows the seq label, kind,
  local time (with RFC3339 in the `<time>` tooltip), the event's own
  hash, and its `prev_hash` (both truncated; hover for full hex).
  Tool events also surface a clickable `call=<id>` link that jumps
  to the next timeline row sharing the same `CallID` — wraps from
  the bottom back to the top.
- **Top**: validation badge. Runs `eventlog.Validate` inline and
  shows ✓ green or ✗ red with a one-line reason ("merkle root
  mismatch", "prev-hash mismatch at index 5", "last event kind … is
  not terminal").

The pretty-printed JSON payload is `event.ToJSON(ev)` indented with
two spaces. Byte-slice fields (hashes, raw provider responses,
CBOR-encoded tool args) currently render as base64 strings — that's
how Go's `encoding/json` handles `[]byte`. Decoding inner CBOR for
known shapes is on the post-M4 list.

## Security model

Read everything below if you're tempted to expose the inspector
beyond `localhost`.

- **Read-only by construction.** The binary opens its database with
  `eventlog.WithReadOnly()` unconditionally. There is no flag to
  switch this off; there is no code path in `cmd/starling-inspect`
  that calls `EventLog.Append`. An inspector cannot mutate the log
  it is inspecting, even by accident.
- **Loopback by default.** `--addr` defaults to `127.0.0.1:0`. The
  inspector has **no authentication**. Binding to a non-loopback
  interface puts the entire event log — every prompt, every
  tool argument, every assistant response — in plaintext in front of
  anyone who can reach the port.
- **If you need remote access**, terminate auth in a reverse proxy
  (nginx, Caddy, Cloudflare Access, tailscale serve) and bind the
  inspector to loopback. Do not skip this step.
- **Live writes are visible.** `WithReadOnly()` opens the SQLite file
  with `?mode=ro` (no `immutable=1`), so a Starling process actively
  writing to the same DB stays correct: the inspector sees new rows
  on the next read. Pinned by `TestSQLite_ReadOnly_SeesLiveWrites`.

## What's intentionally missing in M4

| | Why |
|---|---|
| Replay-from-UI | Needs the user's `Agent` factory; unsolved UX. The Go API `(*Agent).Replay` exists for users who script it. M5+. |
| Live tail of in-progress runs | The current page is a snapshot. Server-Sent Events on top of `eventlog.Stream` is straightforward; deferred until a real user asks. |
| Authentication / TLS | Localhost developer tool by design. Use a reverse proxy. |
| Hash-chain visualization | The validation badge + one-line reason covers the operator workflow. Full visualization is M5+ if demand surfaces. |
| Static export (`starling-inspect export ./out/`) | Cool for postmortems, deferred. |

## Architecture notes

For anyone hacking on the inspector:

- `cmd/starling-inspect/main.go` — flag parsing, listener, signal-driven shutdown.
- `cmd/starling-inspect/server.go` — `//go:embed ui` + route table.
- `cmd/starling-inspect/handlers.go` — HTTP handlers, all thin.
- `cmd/starling-inspect/view.go` — pure-function view models. No HTTP, no IO. Most behavior lives here and is unit-tested.
- `cmd/starling-inspect/templates.go` — `html/template` parsing; pages share `ui/layout.html`, partials (HTMX swaps) parse standalone.
- `cmd/starling-inspect/ui/` — every embedded asset.

Tests:

```sh
go test -race ./cmd/starling-inspect/...
```

Covers the wildcard-URL normalization (`browserURL`), and the pure-
function view layer (`statusOf`, `kindFamily`, `summarize`,
`validationFromError`, `shortHex`, `filterByStatus`,
`rowsFromSummaries`, `rowsFromEvents`).

The read-only SQLite contract is pinned in
`eventlog/sqlite_test.go` (`TestSQLite_ReadOnly_SeesLiveWrites`,
`TestSQLite_ReadOnly_RejectsAppend`).
