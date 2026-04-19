// Package obs holds internal observability helpers shared across the
// root starling package and the step helpers. Everything here is a
// thin wrapper over stdlib log/slog — callers see a plain
// *slog.Logger, no custom logger interface leaks out.
package obs

import (
	"io"
	"log/slog"
)

// Attribute keys used across the library. Keeping them as constants
// here (rather than scattered string literals) lets downstream filters
// rely on stable names.
const (
	AttrRunID     = "run_id"
	AttrTurnID    = "turn_id"
	AttrCallID    = "call_id"
	AttrSeq       = "seq"
	AttrKind      = "kind"
	AttrAttempt   = "attempt"
	AttrToolName  = "tool"
	AttrDurMs     = "duration_ms"
	AttrErrType   = "error_type"
	AttrLimit     = "limit"
	AttrCap       = "cap"
	AttrActual    = "actual"
	AttrNamespace = "namespace"
)

// Discard returns a *slog.Logger that silently drops every record.
// Prefer this over nil checks at call sites: library code can always
// assume a non-nil logger.
func Discard() *slog.Logger {
	// Route to io.Discard with Level above anything we emit — the
	// handler short-circuits on Enabled before formatting.
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.Level(127)}))
}

// Resolve picks the logger to use for a run. If user is non-nil it is
// returned as-is; otherwise slog.Default() is returned so library
// output follows the process-wide default handler without any
// explicit wiring. Callers wanting silence should pass Discard().
func Resolve(user *slog.Logger) *slog.Logger {
	if user != nil {
		return user
	}
	return slog.Default()
}
