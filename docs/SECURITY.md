# Security

Operator-facing security guidance. Treat this as the canonical reference;
the inspector and event log will not warn you if you skip these steps.

## Threat model

Three trust boundaries:

| Actor | What they can do | What we defend |
|---|---|---|
| **Operator** | Runs the agent process, owns the DB and provider keys. | Trusted; the runtime assumes operator code is benign. |
| **End user** | Supplies goals and conversation messages that flow through `Agent.Run` / `Agent.Resume`. | Tool inputs, event payloads, replay determinism. |
| **Provider** | LLM API the agent talks to. | Stream chunk validation (`step.ErrInvalidStream`), raw-response hash (`Config.RequireRawResponseHash`). |

Out of scope:

- Untrusted operator code. The runtime gives operator-level Go callers
  full control of the event log and provider config.
- A compromised provider that wants to influence the audit log. The
  hash chain and Merkle root prove tampering *after* events are
  committed, not the truthfulness of provider-supplied content.

## What the hash chain proves and does not prove

Proves:

- Events were appended in a specific order.
- No event was modified after append (PrevHash + Merkle root).
- Replays match recorded byte-exact behavior.

Does **not** prove:

- That the operator wrote the truth into the log. An operator can
  construct any valid run.
- That the provider returned a specific response. `RawResponseHash` is
  a provider-supplied digest; it commits the operator to *what it
  observed*, not what the provider will return on a future call.
- That the agent ran on the claimed wall-clock time. `Timestamp` is
  drawn from `step.Now`, which can be replayed or mocked.

If you need cross-process attestation, sign the terminal `RunCompleted`
payload externally (the Merkle root is the natural signing target).

## Inspector

### Auth

Bearer auth via `inspect.WithAuth(inspect.BearerAuth(token))` or the
`STARLING_INSPECT_TOKEN` env var. No auth means no auth — never expose
the inspector beyond localhost without a token.

```go
inspect.WithAuth(inspect.BearerAuth(os.Getenv("STARLING_INSPECT_TOKEN")))
```

### CSRF

The inspector plants an `X-CSRF-Token` cookie on safe responses and
requires the same value in the header for every state-changing request
(`POST`, `PUT`, `PATCH`, `DELETE`). The browser-shipped JS handles this
automatically. Custom clients must seed a `GET` first to obtain the
cookie, then echo it in the header.

### TLS

The built-in HTTP server is plain HTTP. Always front it with a reverse
proxy that terminates TLS:

```nginx
server {
    listen 443 ssl;
    ssl_certificate     /etc/ssl/starling.pem;
    ssl_certificate_key /etc/ssl/starling.key;

    location / {
        proxy_pass         http://127.0.0.1:8080;
        proxy_set_header   Host $host;
        proxy_set_header   X-Forwarded-For $remote_addr;
        proxy_http_version 1.1;
        proxy_buffering    off;             # required for SSE
        proxy_read_timeout 1h;
    }
}
```

For mTLS, use `ssl_verify_client on` and supply the CA bundle via
`ssl_client_certificate`. The inspector itself does not inspect client
certs; treat the proxy decision as authoritative.

## Secrets

| Secret | Where | Notes |
|---|---|---|
| Provider API keys | `Agent.Provider` config | Pass via env, not source. Never log. |
| `STARLING_INSPECT_TOKEN` | Env var | Rotate on operator changes. |
| Postgres DSN | Env var (e.g. `DATABASE_URL`) | Use a role with minimum required privileges (`SELECT, INSERT` for the writer; `SELECT` only for the inspector). |

Keys leak through logs more than anywhere else. The default `Logger`
(slog) does not redact provider request payloads — keep API keys in
HTTP headers (where adapters put them) and out of `Config.Params`.

## Sensitive event payloads

Event payloads can contain the entire user/assistant conversation,
reasoning, and tool I/O. Treat the database file or table as you would
treat any other store of user PII:

- Encrypt at rest (filesystem-level for SQLite; pgcrypto / TDE for
  Postgres).
- Restrict access at the OS level. The SQLite file should be `0600`
  owned by the agent user.
- The inspector serves payloads verbatim — never expose it past a
  trusted operator audience.

There is no built-in field-level redaction yet; redact at the tool
boundary if you must keep specific values out of the log. The
event-log boundary redaction hook is on the W9 roadmap.

## Read-only inspector handles

`eventlog.WithReadOnly()` (SQLite) and `eventlog.WithReadOnlyPG()`
(Postgres) gate writes at the application layer. Defence in depth, not
a substitute for OS / DB-level perms:

- SQLite: open the file in read-only mode and chmod `0440`.
- Postgres: connect as a role with only `SELECT` on `eventlog_events`.

## Tool sandboxing

Tools run in-process with full Go capabilities. If a tool shells out,
hits a network, or touches the filesystem, the operator owns the
sandbox: contexts, syscalls allow-list, network egress policy, etc.
The runtime does not constrain tool execution beyond timeouts and
output-size caps configured at the agent level.

## Reporting

Mail security issues privately before opening a public issue. Include a
runnable repro and the affected git SHA.
