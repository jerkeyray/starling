package eventlog

import (
	"strings"
	"time"

	"github.com/jerkeyray/starling/event"
)

func normalizeRunPageOptions(opts RunPageOptions) RunPageOptions {
	if opts.Offset < 0 {
		opts.Offset = 0
	}
	opts.Status = strings.TrimSpace(strings.ToLower(opts.Status))
	opts.Query = strings.TrimSpace(strings.ToLower(opts.Query))
	return opts
}

func runSummaryMatches(s RunSummary, opts RunPageOptions) bool {
	if opts.Status != "" && runStatusLabel(s.TerminalKind) != opts.Status {
		return false
	}
	if !opts.StartedAfter.IsZero() && !s.StartedAt.After(opts.StartedAfter) {
		return false
	}
	if opts.RequireToolCalls && s.ToolCallCount == 0 {
		return false
	}
	if opts.Query != "" && !summaryMatchesQuery(s, opts.Query) {
		return false
	}
	return true
}

func summaryMatchesQuery(s RunSummary, q string) bool {
	return strings.Contains(strings.ToLower(s.RunID), q) ||
		strings.Contains(runStatusLabel(s.TerminalKind), q)
}

func runStatusLabel(k event.Kind) string {
	switch k {
	case event.KindRunCompleted:
		return "completed"
	case event.KindRunFailed:
		return "failed"
	case event.KindRunCancelled:
		return "cancelled"
	default:
		return "in progress"
	}
}

func runStatusKinds(status string) (terminal []event.Kind, inProgress bool, ok bool) {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "":
		return nil, false, true
	case "completed":
		return []event.Kind{event.KindRunCompleted}, false, true
	case "failed":
		return []event.Kind{event.KindRunFailed}, false, true
	case "cancelled":
		return []event.Kind{event.KindRunCancelled}, false, true
	case "in progress":
		return nil, true, true
	default:
		return nil, false, false
	}
}

func runQueryStatusClauses(q string) (terminal []event.Kind, inProgress bool) {
	for _, status := range []string{"completed", "failed", "cancelled", "in progress"} {
		if strings.Contains(status, q) {
			kinds, inp, ok := runStatusKinds(status)
			if !ok {
				continue
			}
			terminal = append(terminal, kinds...)
			inProgress = inProgress || inp
		}
	}
	return terminal, inProgress
}

func paginateRunSummaries(in []RunSummary, opts RunPageOptions) RunPage {
	opts = normalizeRunPageOptions(opts)
	filtered := make([]RunSummary, 0, len(in))
	for _, s := range in {
		if runSummaryMatches(s, opts) {
			filtered = append(filtered, s)
		}
	}
	total := len(filtered)
	start := opts.Offset
	if start > total {
		start = total
	}
	end := total
	if opts.Limit > 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}
	out := make([]RunSummary, end-start)
	copy(out, filtered[start:end])
	return RunPage{Runs: out, TotalMatching: total, Limit: opts.Limit, Offset: opts.Offset}
}

func runStartedAfterUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
