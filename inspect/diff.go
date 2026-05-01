package inspect

import (
	"context"
	"html/template"
	"net/http"

	"github.com/jerkeyray/starling/event"
)

// handleDiff renders a side-by-side diff between two runs, aligned by
// sequence number. It serves /diff?a=<runIDA>&b=<runIDB>.
//
// The diff is event-level: same-seq events are rendered side by side
// with their payloads as canonical JSON (the same projection the
// detail pane uses). A row is "matching" iff the two events have the
// same Kind and byte-identical canonical-JSON payloads. Missing
// indices on either side are surfaced as one-sided rows.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	a := r.URL.Query().Get("a")
	b := r.URL.Query().Get("b")

	// Always populate the picker dropdowns — even on the populated
	// page, the user might want to swap one side without retyping
	// the other.
	options := s.diffOptions(r.Context())

	if a == "" || b == "" {
		s.tpl.render(w, "diff.html", http.StatusOK, s.applyChrome(map[string]any{
			"Title": "Diff runs",
			"A":     a, "B": b,
			"Empty":   true,
			"Options": options,
		}, "diff"))
		return
	}
	evsA, err := s.store.Read(r.Context(), a)
	if err != nil {
		http.Error(w, "read run a: "+err.Error(), http.StatusInternalServerError)
		return
	}
	evsB, err := s.store.Read(r.Context(), b)
	if err != nil {
		http.Error(w, "read run b: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows, summary := buildDiffRows(evsA, evsB)
	s.tpl.render(w, "diff.html", http.StatusOK, s.applyChrome(map[string]any{
		"Title":   "Diff",
		"A":       a,
		"B":       b,
		"Rows":    rows,
		"Summary": summary,
		"Options": options,
	}, "diff"))
}

// diffOption is one entry in the diff page's run dropdown.
type diffOption struct {
	RunID string
	Label string // e.g. "01KQJ260… · 2026-05-01 20:53 · completed"
}

// diffOptions builds the option list for the diff page's two
// run-pickers. Best-effort: a backend that errors out returns nil
// and the template falls back to free-form text inputs.
func (s *Server) diffOptions(ctx context.Context) []diffOption {
	if s.lister == nil {
		return nil
	}
	runs, err := s.lister.ListRuns(ctx)
	if err != nil {
		return nil
	}
	out := make([]diffOption, len(runs))
	for i, rs := range runs {
		label, _ := statusOf(rs.TerminalKind)
		ts := rs.StartedAt.Local().Format("2006-01-02 15:04")
		shortID := rs.RunID
		if len(shortID) > 12 {
			shortID = shortID[:12] + "…"
		}
		out[i] = diffOption{
			RunID: rs.RunID,
			Label: shortID + " · " + ts + " · " + label,
		}
	}
	return out
}

// diffRow is one aligned event pair on the diff page.
type diffRow struct {
	Seq       uint64
	KindA     string
	KindB     string
	BodyA     string // raw pretty-JSON; equality test uses this
	BodyB     string
	BodyAHTML template.HTML // syntax-highlighted form for the template
	BodyBHTML template.HTML
	Class     string // "match" | "diff" | "only-a" | "only-b"
	HasA      bool
	HasB      bool
	OnlyKind  bool // only kinds disagree, not the body bytes
}

type diffSummary struct {
	Total       int
	Matching    int
	Diverging   int
	OnlyA       int
	OnlyB       int
	FirstDivSeq uint64 // 0 if everything matches
}

// buildDiffRows aligns two event slices by sequence number and renders
// each pair as a diffRow. Caller is responsible for ordering — the
// runtime only ever appends in monotonic seq order, so we can walk
// in lockstep.
func buildDiffRows(a, b []event.Event) ([]diffRow, diffSummary) {
	idx := func(evs []event.Event, seq uint64) (event.Event, bool) {
		for i := range evs {
			if evs[i].Seq == seq {
				return evs[i], true
			}
		}
		return event.Event{}, false
	}
	maxSeq := func() uint64 {
		var m uint64
		if n := len(a); n > 0 && a[n-1].Seq > m {
			m = a[n-1].Seq
		}
		if n := len(b); n > 0 && b[n-1].Seq > m {
			m = b[n-1].Seq
		}
		return m
	}()

	out := make([]diffRow, 0, maxSeq)
	var summary diffSummary
	for seq := uint64(1); seq <= maxSeq; seq++ {
		evA, okA := idx(a, seq)
		evB, okB := idx(b, seq)
		row := diffRow{Seq: seq, HasA: okA, HasB: okB}
		switch {
		case okA && okB:
			row.KindA = evA.Kind.String()
			row.KindB = evB.Kind.String()
			row.BodyA = prettyJSON(evA)
			row.BodyB = prettyJSON(evB)
			row.BodyAHTML = highlightJSON(row.BodyA)
			row.BodyBHTML = highlightJSON(row.BodyB)
			switch {
			case row.KindA != row.KindB:
				row.Class = "diff"
				row.OnlyKind = false
				summary.Diverging++
				if summary.FirstDivSeq == 0 {
					summary.FirstDivSeq = seq
				}
			case row.BodyA != row.BodyB:
				row.Class = "diff"
				summary.Diverging++
				if summary.FirstDivSeq == 0 {
					summary.FirstDivSeq = seq
				}
			default:
				row.Class = "match"
				summary.Matching++
			}
		case okA:
			row.KindA = evA.Kind.String()
			row.BodyA = prettyJSON(evA)
			row.BodyAHTML = highlightJSON(row.BodyA)
			row.Class = "only-a"
			summary.OnlyA++
			if summary.FirstDivSeq == 0 {
				summary.FirstDivSeq = seq
			}
		case okB:
			row.KindB = evB.Kind.String()
			row.BodyB = prettyJSON(evB)
			row.BodyBHTML = highlightJSON(row.BodyB)
			row.Class = "only-b"
			summary.OnlyB++
			if summary.FirstDivSeq == 0 {
				summary.FirstDivSeq = seq
			}
		}
		out = append(out, row)
		summary.Total++
	}
	return out, summary
}
