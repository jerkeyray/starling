package inspect

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

// handleRuns renders the runs-list landing page. Optional
// ?status=completed|failed|cancelled|in+progress filters server-side.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	summaries, err := s.lister.ListRuns(r.Context())
	if err != nil {
		http.Error(w, "list runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows := rowsFromSummaries(summaries)
	statusFilter := r.URL.Query().Get("status")
	rows = filterByStatus(rows, statusFilter)
	query := r.URL.Query().Get("q")
	rows = filterByQuery(rows, query)
	preset := r.URL.Query().Get("preset")
	rows = filterByPreset(rows, preset, time.Now())

	s.tpl.render(w, "runs.html", http.StatusOK, s.applyChrome(map[string]any{
		"Title":  "Runs",
		"Rows":   rows,
		"Total":  len(summaries),
		"Status": statusFilter,
		"Query":  query,
		"Preset": preset,
		"Totals": dashTotalsFromRows(rows),
	}, "runs"))
}

// handleRun renders a single run's two-pane detail view: timeline on
// the left, event detail on the right (HTMX-swapped). Validation runs
// inline; the badge at the top reflects eventlog.Validate's verdict.
//
// URL: /run/{runID}
//
// runID may itself contain "/" (e.g., Namespace + "/" + ULID); the
// dispatcher in server.go has already isolated it from any suffix
// segments and URL-unescaped it.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request, runID string) {
	if runID == "" {
		http.NotFound(w, r)
		return
	}

	events, err := s.store.Read(r.Context(), runID)
	if err != nil {
		http.Error(w, "read run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(events) == 0 {
		http.NotFound(w, r)
		return
	}

	rows := rowsFromEvents(runID, events)
	if len(rows) > 0 {
		// The initial render highlights the first event — the one whose
		// detail is pre-painted in the right pane. Live-appended rows
		// never get this flag.
		rows[0].Active = true
	}
	validation := validationFromError(eventlog.Validate(events))

	// Live-tail view-model fields. If the run is already terminal at
	// render time, JS skips opening the SSE; otherwise it subscribes
	// with ?since=<lastSeq> so the browser doesn't re-render the rows
	// that came back in this HTML response.
	terminalKind := ""
	if last := events[len(events)-1]; last.Kind.IsTerminal() {
		terminalKind = last.Kind.String()
	}
	lastSeq := events[len(events)-1].Seq

	// Pre-render the first event's detail so the right pane is not
	// empty on first paint. HTMX swaps it on subsequent clicks.
	initial := detailFromEvent(runID, events[0])

	// Per-run summary header: same numbers the dashboard shows for
	// this row, computed once so the run page is a self-contained view.
	turns, tools, inTok, outTok, cost, durNs := aggregateRunForView(events)
	summary := runSummary{
		EventCount:    uint64(len(events)),
		TurnCount:     turns,
		ToolCallCount: tools,
		InputTokens:   inTok,
		OutputTokens:  outTok,
		CostUSD:       cost,
		DurationMs:    durNs / 1_000_000,
	}

	s.tpl.render(w, "run.html", http.StatusOK, s.applyChrome(map[string]any{
		"Title":         "Run " + runID,
		"RunID":         runID,
		"Rows":          rows,
		"Validation":    validation,
		"Initial":       initial,
		"ReplayEnabled": s.ReplayEnabled(),
		"TerminalKind":  terminalKind,
		"LastSeq":       lastSeq,
		"Summary":       summary,
	}, "runs"))
}

// runSummary backs the top-of-page totals strip on the run detail
// view. Same shape as the dashboard's per-row aggregates.
type runSummary struct {
	EventCount    uint64
	TurnCount     int
	ToolCallCount int
	InputTokens   int64
	OutputTokens  int64
	CostUSD       float64
	DurationMs    int64
}

// aggregateRunForView is the inspector-side counterpart of
// eventlog.aggregateRun. We don't import the unexported helper to
// avoid widening the eventlog public surface for one consumer; the
// duplication is small and stable.
func aggregateRunForView(evs []event.Event) (turns, tools int, inputTokens, outputTokens int64, costUSD float64, durationNs int64) {
	if len(evs) == 0 {
		return
	}
	first := evs[0]
	last := evs[len(evs)-1]
	if last.Timestamp >= first.Timestamp {
		durationNs = last.Timestamp - first.Timestamp
	}
	for i := range evs {
		switch evs[i].Kind {
		case event.KindTurnStarted:
			turns++
		case event.KindToolCallScheduled:
			tools++
		case event.KindAssistantMessageCompleted:
			amc, err := evs[i].AsAssistantMessageCompleted()
			if err != nil {
				continue
			}
			inputTokens += amc.InputTokens
			outputTokens += amc.OutputTokens
			costUSD += amc.CostUSD
		}
	}
	return
}

// handleEventDetail returns the HTML fragment for the right pane.
// Designed for HTMX hx-get; not a full page (no layout).
//
// URL: /run/{runID}/event/{seq}
//
// runID and seqStr come pre-parsed from the dispatcher (runID may
// contain "/" for namespaced runs).
func (s *Server) handleEventDetail(w http.ResponseWriter, r *http.Request, runID, seqStr string) {
	if runID == "" || seqStr == "" {
		http.NotFound(w, r)
		return
	}
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil || seq == 0 {
		http.NotFound(w, r)
		return
	}

	events, err := s.store.Read(r.Context(), runID)
	if err != nil {
		http.Error(w, "read run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if seq > uint64(len(events)) {
		http.NotFound(w, r)
		return
	}
	detail := detailFromEvent(runID, events[seq-1])
	s.tpl.renderPartial(w, "event_detail.html", http.StatusOK, detail)
}
