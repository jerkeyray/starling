// Package replay runs a recorded event log back through the agent loop and
// verifies the reproduced command sequence matches the log. Two modes:
// state-faithful (default) reads recorded LLM and tool outputs from the log;
// re-execution (opt-in) re-invokes providers via a response cache.
package replay
