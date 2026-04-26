# Starling inspector — local web UI

The inspector opens a Starling event log and serves a self-contained
web UI on localhost for browsing runs, walking the event timeline,
inspecting payloads, seeing whether the hash chain validates, and
(optionally) replaying a run side-by-side against your current agent
configuration.

It exists because once a run has more than ~50 events, the CLI dump
stops fitting in a terminal. The UI is pprof-shaped: runs list →
event timeline → detail pane → validation badge. No telemetry, no
CDN; HTMX is vendored, CSS is hand-rolled, templates ship in the
binary via `go:embed`.

There are two ways to run it:

| Mode | Replay-from-UI | How |
|---|---|---|
| **View-only** | No (button hidden) | `starling-inspect path/to/runs.db` |
| **Full (replay enabled)** | Yes | Embed `starling.InspectCommand(factory)` in your own binary and pass a factory that builds your `*starling.Agent` |

View-only mode is the right default for "poke around my audit log."
Full mode is how you use the replay differentiator — the inspector
calls your factory, builds a fresh agent, and streams its events
alongside the recording so you can see exactly where behaviour
diverged.

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
| `--token=<value>` | empty | Require `Authorization: Bearer <value>` on every request. Precedence is `InspectCmd.Token`, then `--token`, then `STARLING_INSPECT_TOKEN`. |

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
known shapes is deferred follow-up work.

## Security model

Read everything below if you're tempted to expose the inspector
beyond `localhost`.

- **Read-only by construction.** The binary opens its database with
  `eventlog.WithReadOnly()` unconditionally. There is no flag to
  switch this off; there is no code path in `cmd/starling-inspect`
  that calls `EventLog.Append`. An inspector cannot mutate the log
  it is inspecting, even by accident.
- **Loopback by default.** `--addr` defaults to `127.0.0.1:0`. The
  inspector ships with auth **off** — binding to a non-loopback
  interface without setting a token puts the entire event log (every
  prompt, every tool argument, every assistant response) in
  plaintext in front of anyone who can reach the port.
- **Bearer-token auth** (`--token=<s>` / `STARLING_INSPECT_TOKEN=<s>` /
  `inspect.WithAuth(fn)`) gates every route — pages, the live-tail
  SSE, static assets, and the replay POSTs — with a constant-time
  bearer comparison. Clients send `Authorization: Bearer <token>`;
  unauthenticated requests get 401 with
  `WWW-Authenticate: Bearer realm="starling-inspect"`. `WithAuth`
  accepts any `func(*http.Request) bool`, so JWT / mTLS / IP-allowlist
  policies drop in without a core change.
- **CSRF protection is always on** for the two replay POST endpoints
  (`POST /run/{id}/replay`, `POST /run/{id}/replay/{session}/control`).
  Double-submit cookie: any GET plants `starling_csrf=<random>`
  (`SameSite=Strict`, not HttpOnly); `replay.js` echoes it into the
  `X-CSRF-Token` header on every POST; a scripted client must do the
  same (one GET to seed, then POST with both the cookie and the
  header).
- **If you need remote access**, set a token and terminate TLS in a
  reverse proxy (nginx, Caddy, Cloudflare Access, tailscale serve).
  The in-process auth is not a substitute for TLS.
- **Live writes are visible.** `WithReadOnly()` opens the SQLite file
  with `?mode=ro` (no `immutable=1`), so a Starling process actively
  writing to the same DB stays correct: the inspector sees new rows
  on the next read. Pinned by `TestSQLite_ReadOnly_SeesLiveWrites`.

### Production mode

For deployments past `localhost`, treat the inspector as an internal
admin surface and front it with infrastructure that handles TLS,
auth, and audit logging. Recommended pattern:

1. **TLS-terminating reverse proxy.** nginx / Caddy / cloud LB owns
   the public certificate; the inspector binds to `127.0.0.1:8080` or
   a Unix socket inside the trust boundary.
2. **Bearer token always set.** Generate via `openssl rand -hex 32`,
   inject through `STARLING_INSPECT_TOKEN`, rotate when operators
   change. Empty tokens are a misconfiguration — the inspector logs a
   loud warning at startup if `--addr` is non-loopback and no
   token/auth handler is set.
3. **mTLS at the proxy** for stronger access control. The inspector
   does not consume client certs; the proxy decision is authoritative.
   See `docs/SECURITY.md` for an nginx recipe.
4. **SSE-friendly proxy config.** Live tail and replay both use
   long-lived SSE connections. Disable buffering and bump
   `proxy_read_timeout` (nginx) / equivalent. Without this, the
   inspector appears to hang after a few seconds.
5. **Cookie scope.** The CSRF cookie is `SameSite=Strict` and not
   marked `Secure`. When serving over HTTPS, terminate at the proxy
   and let the proxy upgrade the cookie via a Set-Cookie rewrite, or
   wrap the inspector handler in middleware that flips `Secure: true`
   on the response.
6. **Request logging.** The inspector itself does not log requests.
   Rely on the proxy access log; pipe it to the same audit pipeline
   that ingests the event log.
7. **Session limits.** A single replay session pins one re-execution
   loop; a hostile or buggy operator can spawn unbounded sessions.
   Cap concurrent connections at the proxy until session-quota
   support lands in-tree.
8. **Inspector metrics.** Not yet exposed. Front-of-proxy metrics
   (request count, latency, byte counts) are the immediate handle.

### Mobile / on-call

The inspector layout collapses to a single column below 900 px and
re-stacks the timeline event row at 600 px. Replay is usable on phone
widths but the parallel-column visualization shrinks; prefer landscape
for divergence triage.

## Live tail

The run-detail page auto-appends events for runs that are still in
progress. When you open `/run/{id}` and the log's last event is not
terminal, the page subscribes to Server-Sent Events at:

```
GET /run/{id}/events/stream?since=<lastSeq>
```

Each frame carries the server-rendered `<li>` row plus a small
metadata envelope:

```json
{"seq": 12, "kind": "ToolCallCompleted", "terminal": false,
 "row_html": "<li data-seq=\"12\" …>…</li>"}
```

The browser appends `row_html` to the timeline as frames arrive. On
the terminal event the page reloads so the server re-runs
`eventlog.Validate` and repaints the hash-chain badge.

The handler does its own catch-up via `store.Read()` before switching
to `store.Stream()` for the tail, so it's correct on every backend
regardless of Stream's history-vs-live ordering guarantees. Runs that
are already terminal at page load open no stream.

Cross-process works out of the box on SQLite: one process appends,
the inspector (opened with `WithReadOnly()`) polls at 50ms and picks
up new rows. Open a run mid-flight, watch it land.

## Full mode: replay from your own binary

The standalone `starling-inspect` binary cannot replay, because
replay needs to construct your `*starling.Agent` — same `Provider`,
same `Tools`, same `Config` — and the binary has no way to know
your agent wiring. Dual-mode solves this: you add an `inspect`
subcommand to your own agent binary, wire `starling.InspectCommand`
with a factory that builds your agent, and the inspector calls back
into your factory when the user clicks Replay.

Minimal skeleton (full working version in `examples/m1_hello`):

```go
func main() {
    if len(os.Args) > 1 && os.Args[1] == "inspect" {
        factory := replay.Factory(func(ctx context.Context) (replay.Agent, error) {
            return buildAgent(ctx) // same function the run path uses
        })
        cmd := starling.InspectCommand(factory)
        if err := cmd.Run(os.Args[2:]); err != nil {
            log.Fatal(err)
        }
        return
    }
    // ... normal agent run ...
}
```

The whole thesis: the factory is literally the same function that
built the original run. That's what keeps replay faithful.

Try it:

```sh
OPENAI_API_KEY=sk-... go run ./examples/m1_hello run
go run ./examples/m1_hello inspect ./runs.db
```

## What's intentionally missing

| | Why |
|---|---|
| Postgres live tail | The SSE endpoint is SQLite-first in the docs because `starling-inspect` opens SQLite files directly. The reusable `inspect.Server` works with any `eventlog.EventLog` + `RunLister`, including Postgres-backed deployments wired by the caller. |
| Authentication / TLS | Localhost developer tool by design. Use a reverse proxy. |
| Hash-chain visualization | The validation badge + one-line reason covers the operator workflow. Full visualization if demand surfaces. |
| Static export (`starling-inspect export ./out/`) | Cool for postmortems, deferred. |

## Architecture notes

For anyone hacking on the inspector:

- `cmd/starling-inspect/main.go` — thin shim; calls `starling.InspectCommand(nil)`.
- `inspect_command.go` (root package) — `InspectCommand` / `InspectCmd`: flag parsing, listener, browser-open, signal-driven shutdown. Also exposed as a library entrypoint for dual-mode binaries.
- `inspect/server.go` — `//go:embed ui` + route table + suffix-aware dispatcher (runIDs may contain `/` from namespaces).
- `inspect/handlers.go` — HTTP handlers for runs list, run detail, event detail.
- `inspect/live.go` — SSE live-tail endpoint (`/run/{id}/events/stream`); `Read()`-snapshot catch-up + `Stream()` tail with seq filter.
- `inspect/replay.go` — replay session lifecycle + SSE streaming.
- `inspect/view.go` — pure-function view models. No HTTP, no IO. Most behavior lives here and is unit-tested.
- `inspect/templates.go` — `html/template` parsing with a `runPath` FuncMap for per-segment URL escaping; pages share `ui/layout.html`, partials parse standalone.
- `inspect/ui/` — every embedded asset (HTML templates, CSS, vendored HTMX).

Tests:

```sh
go test -race ./inspect/... .
```

Covers the wildcard-URL normalization (`browserURL`), namespaced-run
routing, the pure-function view layer, and replay session
lifecycle.

The read-only SQLite contract is pinned in
`eventlog/sqlite_test.go` (`TestSQLite_ReadOnly_SeesLiveWrites`,
`TestSQLite_ReadOnly_RejectsAppend`).
