// Package step is the determinism boundary. Non-deterministic operations
// the agent loop would otherwise perform directly — reading the clock,
// generating randomness, calling the LLM, invoking a tool — go through
// functions in this package so the runtime can record their results in
// the event log and replay them later.
//
// Named "step" (not "runtime") to avoid collision with the stdlib runtime
// package.
package step
