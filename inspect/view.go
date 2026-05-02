package inspect

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

// runPathEscape mirrors the runPath template func: URL-escapes each
// "/"-separated segment of a runID. Used by view-model builders that
// emit URLs (DetailURL, etc.) so namespaced runs route correctly even
// if the namespace contains URL-reserved characters.
func runPathEscape(runID string) string {
	segs := strings.Split(runID, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// runRow is the per-run view model the runs.html template iterates
// over. Keeping a dedicated struct lets the template stay declarative
// (no calls into the event package, no conditional rendering off
// magic Kind integers) and lets the handler pre-compute every label.
type runRow struct {
	RunID       string
	Started     string // formatted timestamp, local time
	StartedISO  string // RFC3339 for the <time> tooltip
	EventCount  uint64 // LastSeq doubles as event count: events are 1-indexed
	StatusLabel string // "completed", "failed", "cancelled", "in progress"
	StatusClass string // CSS class on the badge: "ok", "err", "warn", "muted"

	// Aggregates surfaced from RunSummary. All zero-friendly: a run
	// that hasn't produced an AssistantMessageCompleted yet displays
	// blanks rather than the literal "0 / 0 tok".
	TurnCount     int
	ToolCallCount int
	InputTokens   int64
	OutputTokens  int64
	CostUSD       float64
	DurationMs    int64
}

// dashTotals is the summary header rendered above the runs table.
type dashTotals struct {
	Runs           int
	Turns          int
	ToolCalls      int
	InputTokens    int64
	OutputTokens   int64
	TotalCostUSD   float64
	CompletedRuns  int
	FailedRuns     int
	InProgressRuns int
}

type runsPager struct {
	Page          int
	PerPage       int
	TotalMatching int
	ShowingStart  int
	ShowingEnd    int
	HasPrev       bool
	HasNext       bool
	PrevURL       string
	NextURL       string
}

func dashTotalsFromRows(rows []runRow) dashTotals {
	var t dashTotals
	t.Runs = len(rows)
	for _, r := range rows {
		t.Turns += r.TurnCount
		t.ToolCalls += r.ToolCallCount
		t.InputTokens += r.InputTokens
		t.OutputTokens += r.OutputTokens
		t.TotalCostUSD += r.CostUSD
		switch r.StatusLabel {
		case "completed":
			t.CompletedRuns++
		case "failed":
			t.FailedRuns++
		case "in progress":
			t.InProgressRuns++
		}
	}
	return t
}

func summariesFromRows(rows []runRow) []eventlog.RunSummary {
	out := make([]eventlog.RunSummary, 0, len(rows))
	for _, r := range rows {
		started, _ := time.Parse(time.RFC3339, r.StartedISO)
		out = append(out, eventlog.RunSummary{
			RunID:         r.RunID,
			StartedAt:     started,
			LastSeq:       r.EventCount,
			TerminalKind:  kindForStatusLabel(r.StatusLabel),
			TurnCount:     r.TurnCount,
			ToolCallCount: r.ToolCallCount,
			InputTokens:   r.InputTokens,
			OutputTokens:  r.OutputTokens,
			CostUSD:       r.CostUSD,
			DurationMs:    r.DurationMs,
		})
	}
	return out
}

func kindForStatusLabel(label string) event.Kind {
	switch label {
	case "completed":
		return event.KindRunCompleted
	case "failed":
		return event.KindRunFailed
	case "cancelled":
		return event.KindRunCancelled
	default:
		return event.KindRunStarted
	}
}

// statusOf classifies the run's most recent event into a human label
// + CSS class. Non-terminal kinds all collapse to "in progress" — the
// run hasn't ended yet, even if it's stuck mid-tool-call.
func statusOf(k event.Kind) (label, class string) {
	switch k {
	case event.KindRunCompleted:
		return "completed", "ok"
	case event.KindRunFailed:
		return "failed", "err"
	case event.KindRunCancelled:
		return "cancelled", "warn"
	}
	return "in progress", "muted"
}

// rowsFromSummaries turns a RunLister result into the view-model
// slice the template wants. Pure function — no IO, easy to test.
func rowsFromSummaries(summaries []eventlog.RunSummary) []runRow {
	out := make([]runRow, len(summaries))
	for i, s := range summaries {
		label, class := statusOf(s.TerminalKind)
		out[i] = runRow{
			RunID:         s.RunID,
			Started:       s.StartedAt.Local().Format("2006-01-02 15:04:05"),
			StartedISO:    s.StartedAt.UTC().Format(time.RFC3339),
			EventCount:    s.LastSeq,
			StatusLabel:   label,
			StatusClass:   class,
			TurnCount:     s.TurnCount,
			ToolCallCount: s.ToolCallCount,
			InputTokens:   s.InputTokens,
			OutputTokens:  s.OutputTokens,
			CostUSD:       s.CostUSD,
			DurationMs:    s.DurationMs,
		}
	}
	return out
}

// filterByStatus drops rows whose StatusLabel does not match want.
// An empty want returns rows unchanged. Used for the ?status=...
// query param.
func filterByStatus(rows []runRow, want string) []runRow {
	if want == "" {
		return rows
	}
	out := make([]runRow, 0, len(rows))
	for _, r := range rows {
		if r.StatusLabel == want {
			out = append(out, r)
		}
	}
	return out
}

// filterByQuery does a case-insensitive substring match against
// RunID and StatusLabel. Empty query returns rows unchanged.
func filterByQuery(rows []runRow, q string) []runRow {
	q = strings.TrimSpace(strings.ToLower(q))
	if q == "" {
		return rows
	}
	out := make([]runRow, 0, len(rows))
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.RunID), q) ||
			strings.Contains(strings.ToLower(r.StatusLabel), q) {
			out = append(out, r)
		}
	}
	return out
}

// filterByPreset applies one of the named preset chips. Empty preset
// returns rows unchanged. Recognised: "tools" (rows with at least one
// tool call), "hour" (rows started within the last hour).
func filterByPreset(rows []runRow, preset string, now time.Time) []runRow {
	switch preset {
	case "":
		return rows
	case "tools":
		out := make([]runRow, 0, len(rows))
		for _, r := range rows {
			if r.ToolCallCount > 0 {
				out = append(out, r)
			}
		}
		return out
	case "hour":
		cutoff := now.Add(-time.Hour)
		out := make([]runRow, 0, len(rows))
		for _, r := range rows {
			t, err := time.Parse(time.RFC3339, r.StartedISO)
			if err != nil {
				continue
			}
			if t.After(cutoff) {
				out = append(out, r)
			}
		}
		return out
	}
	return rows
}

func filterRowsStartedAfter(rows []runRow, cutoff time.Time) []runRow {
	out := make([]runRow, 0, len(rows))
	for _, r := range rows {
		t, err := time.Parse(time.RFC3339, r.StartedISO)
		if err != nil {
			continue
		}
		if t.After(cutoff) {
			out = append(out, r)
		}
	}
	return out
}

// eventRow is one row in the timeline pane of the run detail page.
// Pre-computed so the template stays declarative — no calls into the
// event package, no conditional rendering off Kind integers.
type eventRow struct {
	Seq       uint64
	SeqLabel  string // "#0001", padded for visual alignment
	Time      string // local hh:mm:ss.SSS
	Kind      string // event.Kind.String()
	Family    string // CSS class: lifecycle / tool / message / budget / terminal
	Summary   string // short payload-derived label, e.g. "tool=fetch call=c1"
	CallID    string // non-empty for tool events; used for cross-link highlighting
	DetailURL string // "/run/{id}/event/{seq}" for hx-get
	Active    bool   // initial-render highlight; always false for SSE-streamed rows

	// Annotation is a short payload-derived metric chip rendered on
	// the timeline row itself — e.g. "$0.0024 · 1227 in / 30 out · cache 0.83".
	// Empty for kinds with nothing useful to show.
	Annotation string
}

// validationView renders the badge at the top of the run detail page.
// Reason is empty on success.
type validationView struct {
	OK     bool
	Class  string // "ok" or "err"
	Label  string // "chain valid" / "chain invalid"
	Reason string // one-line eventlog.Validate error message; empty when OK
}

// eventDetail is the right-pane payload of /run/{id}/event/{seq}.
type eventDetail struct {
	RunID         string
	Seq           uint64
	SeqLabel      string
	Kind          string
	Family        string
	Time          string
	TimeISO       string
	HashHex       string // hex of event.Hash(event.Marshal(ev)); short + full
	HashShort     string
	PrevHashHex   string
	PrevHashShort string
	CallID        string
	JSON          string        // pretty-printed event.ToJSON output, plain text (used as copy source)
	JSONHTML      template.HTML // pretty-printed JSON with .tok-* spans for syntax highlight
}

// kindFamily groups event kinds by visual category so the timeline
// can color-code them. Returns the CSS class suffix used in app.css
// (.ev-lifecycle, .ev-tool, etc.).
func kindFamily(k event.Kind) string {
	switch k {
	case event.KindRunStarted, event.KindTurnStarted:
		return "lifecycle"
	case event.KindUserMessageAppended,
		event.KindReasoningEmitted,
		event.KindAssistantMessageCompleted:
		return "message"
	case event.KindToolCallScheduled,
		event.KindToolCallCompleted,
		event.KindToolCallFailed,
		event.KindSideEffectRecorded:
		return "tool"
	case event.KindBudgetExceeded, event.KindContextTruncated:
		return "budget"
	case event.KindRunCompleted, event.KindRunFailed, event.KindRunCancelled:
		return "terminal"
	}
	return "unknown"
}

// summarize returns a short one-line label for the timeline. Decoding
// errors collapse to an empty string — the row still renders with just
// the kind name. Keep this table-driven; adding a new kind is one case.
func summarize(ev event.Event) (summary, callID string) {
	switch ev.Kind {
	case event.KindRunStarted:
		if p, err := ev.AsRunStarted(); err == nil {
			return "model=" + p.ModelID, ""
		}
	case event.KindTurnStarted:
		if p, err := ev.AsTurnStarted(); err == nil {
			return fmt.Sprintf("turn=%s", p.TurnID), ""
		}
	case event.KindUserMessageAppended:
		if p, err := ev.AsUserMessageAppended(); err == nil {
			return truncOneLine(p.Content, 60), ""
		}
	case event.KindReasoningEmitted:
		if p, err := ev.AsReasoningEmitted(); err == nil {
			return truncOneLine(p.Content, 60), ""
		}
	case event.KindAssistantMessageCompleted:
		if p, err := ev.AsAssistantMessageCompleted(); err == nil {
			return truncOneLine(p.Text, 60), ""
		}
	case event.KindToolCallScheduled:
		if p, err := ev.AsToolCallScheduled(); err == nil {
			return fmt.Sprintf("tool=%s call=%s", p.ToolName, p.CallID), p.CallID
		}
	case event.KindToolCallCompleted:
		if p, err := ev.AsToolCallCompleted(); err == nil {
			return fmt.Sprintf("call=%s ok %dms", p.CallID, p.DurationMs), p.CallID
		}
	case event.KindToolCallFailed:
		if p, err := ev.AsToolCallFailed(); err == nil {
			return fmt.Sprintf("call=%s err=%s", p.CallID, p.ErrorType), p.CallID
		}
	case event.KindSideEffectRecorded:
		if p, err := ev.AsSideEffectRecorded(); err == nil {
			return "name=" + p.Name, ""
		}
	case event.KindBudgetExceeded:
		if p, err := ev.AsBudgetExceeded(); err == nil {
			return "budget=" + string(p.Limit), ""
		}
	case event.KindContextTruncated:
		return "context truncated", ""
	case event.KindRunCompleted:
		if p, err := ev.AsRunCompleted(); err == nil {
			return fmt.Sprintf("turns=%d tools=%d", p.TurnCount, p.ToolCallCount), ""
		}
	case event.KindRunFailed:
		if p, err := ev.AsRunFailed(); err == nil {
			return string(p.ErrorType) + ": " + truncOneLine(p.Error, 60), ""
		}
	case event.KindRunCancelled:
		if p, err := ev.AsRunCancelled(); err == nil {
			return "reason=" + p.Reason, ""
		}
	}
	return "", ""
}

func truncOneLine(s string, n int) string {
	// Collapse newlines so the timeline stays single-line.
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	if len(out) <= n {
		return string(out)
	}
	return string(out[:n]) + "…"
}

// rowFromEvent builds one timeline row. Pure function; shared by the
// bulk renderer (rowsFromEvents) and the live-tail SSE endpoint so
// the server-side render pipeline is single-sourced.
func rowFromEvent(runID string, ev event.Event) eventRow {
	summary, callID := summarize(ev)
	return eventRow{
		Seq:        ev.Seq,
		SeqLabel:   fmt.Sprintf("#%04d", ev.Seq),
		Time:       time.Unix(0, ev.Timestamp).Local().Format("15:04:05.000"),
		Kind:       ev.Kind.String(),
		Family:     kindFamily(ev.Kind),
		Summary:    summary,
		CallID:     callID,
		DetailURL:  fmt.Sprintf("/run/%s/event/%d", runPathEscape(runID), ev.Seq),
		Annotation: annotateEvent(ev),
	}
}

// annotateEvent returns a short metric chip for ev, or "" if there's
// nothing useful to surface. Currently populated for
// AssistantMessageCompleted (cost / tokens / cache hit ratio) and the
// terminal kinds (run-level cost / duration when available).
func annotateEvent(ev event.Event) string {
	switch ev.Kind {
	case event.KindAssistantMessageCompleted:
		amc, err := ev.AsAssistantMessageCompleted()
		if err != nil {
			return ""
		}
		parts := []string{
			fmt.Sprintf("$%.4f", amc.CostUSD),
			fmt.Sprintf("%d in / %d out", amc.InputTokens, amc.OutputTokens),
		}
		if total := amc.CacheReadTokens + amc.InputTokens; total > 0 && amc.CacheReadTokens > 0 {
			parts = append(parts, fmt.Sprintf("cache %.2f", float64(amc.CacheReadTokens)/float64(total)))
		}
		return strings.Join(parts, " · ")
	}
	return ""
}

// rowsFromEvents builds the timeline. Pure function.
func rowsFromEvents(runID string, events []event.Event) []eventRow {
	out := make([]eventRow, len(events))
	for i, ev := range events {
		out[i] = rowFromEvent(runID, ev)
	}
	return out
}

// detailFromEvent builds the right-pane view model for a single event.
// JSON is pretty-printed; on error we fall back to a stub so the page
// still renders.
func detailFromEvent(runID string, ev event.Event) eventDetail {
	_, callID := summarize(ev)

	// Compute this event's hash so the detail header can show it (the
	// next event's PrevHash should equal it).
	var hashHex, hashShort string
	if enc, err := event.Marshal(ev); err == nil {
		h := event.Hash(enc)
		hashHex = hex.EncodeToString(h)
		hashShort = shortHex(hashHex)
	}

	prevHex := hex.EncodeToString(ev.PrevHash)
	prevShort := shortHex(prevHex)
	if prevHex == "" {
		prevHex = "(genesis — no predecessor)"
		prevShort = prevHex
	}

	pretty := prettyJSON(ev)
	return eventDetail{
		RunID:         runID,
		Seq:           ev.Seq,
		SeqLabel:      fmt.Sprintf("#%04d", ev.Seq),
		Kind:          ev.Kind.String(),
		Family:        kindFamily(ev.Kind),
		Time:          time.Unix(0, ev.Timestamp).Local().Format("15:04:05.000"),
		TimeISO:       time.Unix(0, ev.Timestamp).UTC().Format(time.RFC3339Nano),
		HashHex:       hashHex,
		HashShort:     hashShort,
		PrevHashHex:   prevHex,
		PrevHashShort: prevShort,
		CallID:        callID,
		JSON:          pretty,
		JSONHTML:      highlightJSON(pretty),
	}
}

func shortHex(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}

// prettyJSON returns a 2-space-indented JSON dump of the event payload
// (via event.ToJSON). On error returns a placeholder so the page
// always renders something.
func prettyJSON(ev event.Event) string {
	raw, err := event.ToJSON(ev)
	if err != nil {
		return fmt.Sprintf("// could not decode payload: %v", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		// Fall back to the raw projection rather than crashing.
		return string(raw)
	}
	return buf.String()
}

// highlightJSON wraps the tokens of a pretty-printed JSON document
// in span elements (.tok-key / .tok-string / .tok-num / .tok-bool /
// .tok-null / .tok-punct) so CSS can color them. Server-side so the
// inspector stays JS-free; uses a small hand-rolled scanner because
// encoding/json doesn't expose tokens with their original whitespace.
//
// Input is the output of prettyJSON (already indented). Non-JSON
// fallback strings ("// could not decode payload: ...") are escaped
// as plain text so a decode error still renders safely.
func highlightJSON(pretty string) template.HTML {
	if pretty == "" {
		return ""
	}
	if !strings.HasPrefix(pretty, "{") && !strings.HasPrefix(pretty, "[") {
		return template.HTML(html.EscapeString(pretty))
	}

	var b strings.Builder
	b.Grow(len(pretty) + len(pretty)/4)

	i := 0
	for i < len(pretty) {
		c := pretty[i]
		switch {
		case c == '"':
			// Scan to the closing quote, honoring backslash escapes.
			j := i + 1
			for j < len(pretty) {
				if pretty[j] == '\\' && j+1 < len(pretty) {
					j += 2
					continue
				}
				if pretty[j] == '"' {
					j++
					break
				}
				j++
			}
			tok := pretty[i:j]
			cls := "tok-string"
			// Keys are followed (after optional whitespace) by ':'.
			k := j
			for k < len(pretty) && (pretty[k] == ' ' || pretty[k] == '\t') {
				k++
			}
			if k < len(pretty) && pretty[k] == ':' {
				cls = "tok-key"
			}
			b.WriteString(`<span class="`)
			b.WriteString(cls)
			b.WriteString(`">`)
			b.WriteString(html.EscapeString(tok))
			b.WriteString(`</span>`)
			i = j
		case c == '{' || c == '}' || c == '[' || c == ']' || c == ',' || c == ':':
			b.WriteString(`<span class="tok-punct">`)
			b.WriteByte(c)
			b.WriteString(`</span>`)
			i++
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(pretty) {
				d := pretty[j]
				if (d >= '0' && d <= '9') || d == '.' || d == 'e' || d == 'E' || d == '+' || d == '-' {
					j++
					continue
				}
				break
			}
			b.WriteString(`<span class="tok-num">`)
			b.WriteString(html.EscapeString(pretty[i:j]))
			b.WriteString(`</span>`)
			i = j
		case c == 't' && strings.HasPrefix(pretty[i:], "true"):
			b.WriteString(`<span class="tok-bool">true</span>`)
			i += 4
		case c == 'f' && strings.HasPrefix(pretty[i:], "false"):
			b.WriteString(`<span class="tok-bool">false</span>`)
			i += 5
		case c == 'n' && strings.HasPrefix(pretty[i:], "null"):
			b.WriteString(`<span class="tok-null">null</span>`)
			i += 4
		default:
			// Whitespace and anything else: escape and emit verbatim.
			b.WriteString(html.EscapeString(string(c)))
			i++
		}
	}
	return template.HTML(b.String())
}

// validationFromError turns the result of eventlog.Validate into the
// badge view model. err==nil means valid.
func validationFromError(err error) validationView {
	if err == nil {
		return validationView{OK: true, Class: "ok", Label: "chain valid"}
	}
	reason := err.Error()
	// Strip the wrapping "eventlog: log failed validation: " prefix so
	// the badge subtext is just the diagnostic.
	if errors.Is(err, eventlog.ErrLogCorrupt) {
		const prefix = "eventlog: log failed validation: "
		if len(reason) > len(prefix) && reason[:len(prefix)] == prefix {
			reason = reason[len(prefix):]
		}
	}
	return validationView{OK: false, Class: "err", Label: "chain invalid", Reason: reason}
}
