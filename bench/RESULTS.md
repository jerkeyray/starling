# Benchmark baseline

Run with `go test -bench=. -benchtime=5000x ./bench/`. Numbers below
are illustrative — record fresh ones on the target host before quoting
in production claims.

## 2026-04 — Apple M5, Go 1.23

| Benchmark             | ns/op   | Notes                              |
|-----------------------|---------|------------------------------------|
| Append_InMemory       | ~1,700  | one append, hash chain validated   |
| Append_SQLite         | ~31,000 | WAL mode, busy_timeout=5s          |
| Validate_1k           | ~1.4M   | seq + chain + merkle, 1k events    |
| Validate_10k          | ~7.5M   | linear in event count              |

Take p50/p99 with `-benchtime=10000x` and `-count=5` and feed `ns/op`
into your perf tracker. The bench package is intentionally minimal so
re-running it produces stable numbers across runs.
