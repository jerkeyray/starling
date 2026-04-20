package inspect

import (
	"net/http"
	"strconv"

	"github.com/jerkeyray/starling/eventlog"
)

// handleRuns renders the runs-list landing page. Calls
// RunLister.ListRuns (already cached on the server struct) and turns
// the result into per-row view models so the template stays simple.
//
// Supports a single optional query param:
//
//	?status=completed | failed | cancelled | in+progress
//
// for server-side filtering. Empty / unknown values return all rows.
// Client-side text search is provided by app.js on top of the
// rendered table.
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

	s.tpl.render(w, "runs.html", http.StatusOK, map[string]any{
		"Title":  "Runs",
		"Rows":   rows,
		"Total":  len(summaries),
		"Status": statusFilter,
	})
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

	s.tpl.render(w, "run.html", http.StatusOK, map[string]any{
		"Title":         "Run " + runID,
		"RunID":         runID,
		"Rows":          rows,
		"Validation":    validation,
		"Initial":       initial,
		"ReplayEnabled": s.ReplayEnabled(),
		"TerminalKind":  terminalKind,
		"LastSeq":       lastSeq,
	})
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
