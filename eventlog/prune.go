package eventlog

import (
	"errors"
	"strings"
	"time"

	"github.com/jerkeyray/starling/event"
)

var ErrInvalidPruneOptions = errors.New("eventlog: invalid prune options")

func normalizePruneOptions(opts PruneOptions) PruneOptions {
	opts.Status = strings.TrimSpace(strings.ToLower(opts.Status))
	if opts.Limit < 0 {
		opts.Limit = 0
	}
	return opts
}

func validatePruneOptions(opts PruneOptions) error {
	if opts.Before.IsZero() {
		return ErrInvalidPruneOptions
	}
	if !time.Unix(0, opts.Before.UnixNano()).Equal(opts.Before) {
		return ErrInvalidPruneOptions
	}
	if opts.Status == "" {
		return nil
	}
	if _, _, ok := runStatusKinds(opts.Status); !ok {
		return ErrInvalidPruneOptions
	}
	return nil
}

func pruneSummaryMatches(s RunSummary, opts PruneOptions) bool {
	if !s.StartedAt.Before(opts.Before) {
		return false
	}
	if opts.Status != "" {
		return runStatusLabel(s.TerminalKind) == opts.Status
	}
	if s.TerminalKind.IsTerminal() {
		return true
	}
	return opts.IncludeInProgress
}

func pruneAllowedKinds(opts PruneOptions) (terminal []event.Kind, inProgress bool) {
	if opts.Status != "" {
		terminal, inProgress, _ = runStatusKinds(opts.Status)
		return terminal, inProgress
	}
	terminal = []event.Kind{
		event.KindRunCompleted,
		event.KindRunFailed,
		event.KindRunCancelled,
	}
	return terminal, opts.IncludeInProgress
}
