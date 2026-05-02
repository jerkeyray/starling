package inspect

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

const (
	defaultRunsPageSize = 50
	maxRunsPageSize     = 200
	diffOptionsLimit    = 100
)

// handleRuns renders the runs-list landing page. Optional
// ?status=completed|failed|cancelled|in+progress filters server-side.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	statusFilter := r.URL.Query().Get("status")
	query := r.URL.Query().Get("q")
	preset := r.URL.Query().Get("preset")
	pageNum := parsePositiveInt(r.URL.Query().Get("page"), 1)
	perPage := parsePositiveInt(r.URL.Query().Get("per_page"), defaultRunsPageSize)
	if perPage > maxRunsPageSize {
		perPage = maxRunsPageSize
	}
	offset := (pageNum - 1) * perPage

	opts := eventlog.RunPageOptions{
		Limit:  perPage,
		Offset: offset,
		Status: statusFilter,
		Query:  query,
	}
	switch preset {
	case "tools":
		opts.RequireToolCalls = true
	case "hour":
		opts.StartedAfter = time.Now().Add(-time.Hour)
	}
	page, err := s.listRunPage(r.Context(), opts)
	if err != nil {
		http.Error(w, "list runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(page.Runs) == 0 && page.TotalMatching > 0 && offset >= page.TotalMatching {
		pageNum = (page.TotalMatching + perPage - 1) / perPage
		opts.Offset = (pageNum - 1) * perPage
		page, err = s.listRunPage(r.Context(), opts)
		if err != nil {
			http.Error(w, "list runs: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	rows := rowsFromSummaries(page.Runs)
	pager := runsPager{
		Page:          pageNum,
		PerPage:       perPage,
		TotalMatching: page.TotalMatching,
		ShowingStart:  page.Offset + 1,
		ShowingEnd:    page.Offset + len(rows),
	}
	if len(rows) == 0 {
		pager.ShowingStart = 0
	}
	if page.Offset > 0 {
		pager.HasPrev = true
		pager.PrevURL = runsPageURL(r.URL.Query(), pageNum-1, perPage)
	}
	if page.Offset+len(rows) < page.TotalMatching {
		pager.HasNext = true
		pager.NextURL = runsPageURL(r.URL.Query(), pageNum+1, perPage)
	}

	s.tpl.render(w, "runs.html", http.StatusOK, s.applyChrome(map[string]any{
		"Title":  "Runs",
		"Rows":   rows,
		"Total":  page.TotalMatching,
		"Status": statusFilter,
		"Query":  query,
		"Preset": preset,
		"Totals": dashTotalsFromRows(rows),
		"Pager":  pager,
	}, "runs"))
}

func (s *Server) listRunPage(ctx context.Context, opts eventlog.RunPageOptions) (eventlog.RunPage, error) {
	if pager, ok := s.lister.(eventlog.RunPageLister); ok {
		return pager.ListRunsPage(ctx, opts)
	}
	summaries, err := s.lister.ListRuns(ctx)
	if err != nil {
		return eventlog.RunPage{}, err
	}
	rows := rowsFromSummaries(summaries)
	rows = filterByStatus(rows, opts.Status)
	rows = filterByQuery(rows, opts.Query)
	if opts.RequireToolCalls {
		rows = filterByPreset(rows, "tools", time.Now())
	}
	if !opts.StartedAfter.IsZero() {
		rows = filterRowsStartedAfter(rows, opts.StartedAfter)
	}
	total := len(rows)
	start := opts.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := total
	if opts.Limit > 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}
	return eventlog.RunPage{
		Runs:          summariesFromRows(rows[start:end]),
		TotalMatching: total,
		Limit:         opts.Limit,
		Offset:        start,
	}, nil
}

func parsePositiveInt(raw string, fallback int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func runsPageURL(base url.Values, page, perPage int) string {
	q := cloneValues(base)
	q.Set("page", strconv.Itoa(page))
	if perPage != defaultRunsPageSize {
		q.Set("per_page", strconv.Itoa(perPage))
	} else {
		q.Del("per_page")
	}
	return "/?" + q.Encode()
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
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
