# Contributing to Starling

Thanks for your interest. Starling is small on purpose — the public API
lives in the root package, backends and adapters live in subpackages,
and the bar for new surface area is high. Read this before opening a PR.

## Ground rules

- **Discuss first for non-trivial changes.** Open an issue describing
  the problem before writing code. New public API, new subpackages,
  new dependencies, and anything touching the event schema all need a
  thumbs-up first.
- **No new top-level packages without discussion.** The layout (root
  for public API, `eventlog/`, `provider/`, `tool/`, `replay/`,
  `inspect/`, `cmd/`, `internal/`) is deliberate.
- **No new dependencies without justification.** Every `go.mod` line
  is load-bearing for someone's supply chain.
- **The event schema is append-only.** Existing event kinds and
  payload fields don't change shape. New kinds go through review.

## Dev loop

```
make check
```

That target runs `go vet`, a `gofmt` drift check, `golangci-lint`,
`govulncheck`, and `go test -race -count=10 ./...`. CI runs the same
set plus a Go version matrix (`stable` and the pinned floor in
`go.mod`) and the Postgres backend matrix.

`make check` must pass before a PR is mergeable. If your change is
concurrency-heavy, bump the race count further on the package you
touched — `-count=1` has masked real races in this repo before.

## Style

- Idiomatic Go. Run `gofmt`. No generated code checked in.
- Doc comments on every exported identifier. If you rename something,
  update the doc comment — drift is the easiest way to mislead users.
- Tests next to the code they cover. Table-driven where it helps,
  straight-line where it doesn't.
- Error messages are lowercase, no trailing punctuation, wrap with
  `%w` when the caller might want `errors.Is`.

## Commits & PRs

- One logical change per commit. Conventional-ish prefixes (`feat:`,
  `fix:`, `docs:`, `refactor:`, `test:`) are welcome but not required.
- PR description: what changed, why, how to verify. Link the issue.
- Don't force-push over review comments unless asked — rebase at the
  end.

## What's out of scope

See `temp_docs/ROADMAP.md` §6 for the non-goals list. Prompt DSLs,
vector stores, chain/flow DSLs, HTTP wrappers, and retry policies in
core are all explicit non-goals — PRs adding them will be closed.

