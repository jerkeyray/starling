package eventlog

import (
	"fmt"
	"strings"

	"github.com/jerkeyray/starling/event"
)

type runPageSQLCond struct {
	part string
	args []any
}

func runPageSQLConditions(opts RunPageOptions, nextPlaceholder func() string) []runPageSQLCond {
	var conds []runPageSQLCond
	if opts.Status != "" {
		terminal, inProgress, ok := runStatusKinds(opts.Status)
		conds = append(conds, runStatusSQLCond(terminal, inProgress, ok, nextPlaceholder))
	}
	if opts.Query != "" {
		conds = append(conds, runQuerySQLCond(opts.Query, nextPlaceholder))
	}
	if !opts.StartedAfter.IsZero() {
		conds = append(conds, runPageSQLCond{
			part: "started_ts > " + nextPlaceholder(),
			args: []any{runStartedAfterUnixNano(opts.StartedAfter)},
		})
	}
	if opts.RequireToolCalls {
		conds = append(conds, runPageSQLCond{part: "has_tools"})
	}
	return conds
}

func runStatusSQLCond(terminal []event.Kind, inProgress bool, ok bool, nextPlaceholder func() string) runPageSQLCond {
	if !ok {
		return runPageSQLCond{part: "1 = 0"}
	}
	return runKindsSQLCond("last_kind", terminal, inProgress, nextPlaceholder)
}

func runQuerySQLCond(q string, nextPlaceholder func() string) runPageSQLCond {
	parts := []string{"LOWER(run_id) LIKE " + nextPlaceholder()}
	args := []any{"%" + strings.ToLower(q) + "%"}
	terminal, inProgress := runQueryStatusClauses(q)
	if len(terminal) > 0 || inProgress {
		kindCond := runKindsSQLCond("last_kind", terminal, inProgress, nextPlaceholder)
		parts = append(parts, kindCond.part)
		args = append(args, kindCond.args...)
	}
	return runPageSQLCond{part: "(" + strings.Join(parts, " OR ") + ")", args: args}
}

func runKindsSQLCond(col string, terminal []event.Kind, inProgress bool, nextPlaceholder func() string) runPageSQLCond {
	terms := make([]string, 0, 2)
	var args []any
	if len(terminal) > 0 {
		ph := make([]string, len(terminal))
		for i, k := range terminal {
			ph[i] = nextPlaceholder()
			args = append(args, int64(k))
		}
		terms = append(terms, col+" IN ("+strings.Join(ph, ", ")+")")
	}
	if inProgress {
		ph := make([]string, 3)
		for i, k := range []event.Kind{event.KindRunCompleted, event.KindRunFailed, event.KindRunCancelled} {
			ph[i] = nextPlaceholder()
			args = append(args, int64(k))
		}
		terms = append(terms, col+" NOT IN ("+strings.Join(ph, ", ")+")")
	}
	if len(terms) == 0 {
		return runPageSQLCond{part: "1 = 0"}
	}
	return runPageSQLCond{part: "(" + strings.Join(terms, " OR ") + ")", args: args}
}

func questionPlaceholder() func() string {
	return func() string { return "?" }
}

func numberedPlaceholder(start int) func() string {
	n := start
	return func() string {
		s := fmt.Sprintf("$%d", n)
		n++
		return s
	}
}
