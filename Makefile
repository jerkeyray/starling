# Makefile — convenience targets for local development.
# All targets are thin wrappers over `go` so they work without extra
# tooling. Override DB= to change where the demo log lives.

DB ?= /tmp/starling-demo.db

.PHONY: help build vet test demo-inspect inspect

help:
	@echo "Targets:"
	@echo "  build         - go build ./..."
	@echo "  vet           - go vet ./..."
	@echo "  test          - go test -race ./..."
	@echo "  demo-inspect  - seed $(DB) and launch starling-inspect against it"
	@echo "  inspect       - launch starling-inspect against an existing DB (DB=...)"

build:
	go build ./...

vet:
	go vet ./...

test:
	go test -race ./...

# One-command demo: seed a synthetic SQLite log with four runs (one of
# each terminal status, plus an in-progress run) and open the inspector
# against it. No API keys, no provider, no internet required.
demo-inspect:
	go run ./examples/m4_inspector_demo $(DB)
	go run ./cmd/starling-inspect $(DB)

# Open the inspector against an arbitrary DB. Useful after a real run.
#   make inspect DB=/path/to/runs.db
inspect:
	go run ./cmd/starling-inspect $(DB)
