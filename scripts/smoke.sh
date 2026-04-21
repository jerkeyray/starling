#!/usr/bin/env bash
# smoke.sh — no-API-key end-to-end smoke test.
#
# Seeds a throwaway SQLite log with the m4_inspector_demo synthetic
# runs, exercises the CLI (validate + export), and probes every
# inspector HTTP route. Exits nonzero on the first failure.
#
# Usage:
#   scripts/smoke.sh           # default port 7888, tmp db
#   PORT=7999 scripts/smoke.sh

set -euo pipefail

PORT="${PORT:-7888}"
DB="$(mktemp -d)/smoke.db"
RUN_ID="demo-completed"
INSPECT_PID=""

cleanup() {
  if [[ -n "$INSPECT_PID" ]] && kill -0 "$INSPECT_PID" 2>/dev/null; then
    kill "$INSPECT_PID" 2>/dev/null || true
    wait "$INSPECT_PID" 2>/dev/null || true
  fi
  rm -rf "$(dirname "$DB")"
}
trap cleanup EXIT

step() { printf "\033[1m▸ %s\033[0m\n" "$*"; }
ok()   { printf "  \033[32m✓\033[0m %s\n" "$*"; }
fail() { printf "  \033[31m✗\033[0m %s\n" "$*" >&2; exit 1; }

# ---------- seed -----------------------------------------------------------

step "seed synthetic runs into $DB"
go run ./examples/m4_inspector_demo "$DB" >/dev/null
[[ -s "$DB" ]] || fail "db not written"
ok "db seeded"

# ---------- CLI: validate --------------------------------------------------

step "starling validate (terminal runs)"
# demo-in-progress is intentionally non-terminal in the seeded set, so
# whole-log validate is expected to fail; check the terminal runs one by one.
for rid in demo-completed demo-failed demo-cancelled; do
  if ! go run ./cmd/starling validate "$DB" "$rid" >/tmp/smoke-validate.log 2>&1; then
    cat /tmp/smoke-validate.log >&2
    fail "validate $rid failed"
  fi
  ok "validate $rid"
done

# ---------- CLI: export ----------------------------------------------------

step "starling export $RUN_ID → NDJSON"
EXPORT_OUT="$(go run ./cmd/starling export "$DB" "$RUN_ID" 2>/tmp/smoke-export.err)"
if [[ -z "$EXPORT_OUT" ]]; then
  cat /tmp/smoke-export.err >&2
  fail "export produced no output"
fi
# Each line must parse as JSON.
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  echo "$line" | python3 -c 'import json,sys; json.loads(sys.stdin.read())' >/dev/null \
    || fail "export: invalid JSON line: $line"
done <<<"$EXPORT_OUT"
ok "export produced $(echo "$EXPORT_OUT" | grep -c .) valid JSON lines"

# ---------- inspector HTTP -------------------------------------------------

step "start starling-inspect on :$PORT"
go run ./cmd/starling-inspect -addr "127.0.0.1:$PORT" "$DB" >/tmp/smoke-inspect.log 2>&1 &
INSPECT_PID=$!

# Poll until it binds (up to ~5s).
for i in $(seq 1 100); do
  if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null; then break; fi
  sleep 0.1
done
curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" || {
  cat /tmp/smoke-inspect.log >&2
  fail "inspector did not start"
}
ok "inspector is up"

probe() {
  local path="$1" expect="$2"
  local body
  if ! body="$(curl -fsS "http://127.0.0.1:$PORT$path")"; then
    fail "GET $path: HTTP error"
  fi
  if [[ -n "$expect" ]] && ! grep -q "$expect" <<<"$body"; then
    fail "GET $path: expected substring not found: $expect"
  fi
  ok "GET $path"
}

step "probe HTTP routes"
probe "/"                                     "starling-inspect"
probe "/run/$RUN_ID"                          "$RUN_ID"
probe "/run/$RUN_ID/event/1"                  "RunStarted"
probe "/static/app.css"                       ".timeline"
probe "/static/app.js"                        "initCopyButtons"

# 404 path — should not 500.
code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/run/does-not-exist")"
[[ "$code" == "404" ]] || fail "missing run: expected 404, got $code"
ok "GET /run/does-not-exist → 404"

step "all checks passed"
