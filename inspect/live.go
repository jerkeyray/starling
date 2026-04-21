package inspect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jerkeyray/starling/event"
)

// liveFrame is one SSE payload on the /events/stream endpoint. It
// mirrors the shape replay's SSE frames use — a pre-rendered HTML
// snippet plus enough metadata for the browser to decide whether to
// close the stream.
type liveFrame struct {
	Seq      uint64 `json:"seq"`
	Kind     string `json:"kind"`
	Terminal bool   `json:"terminal"`
	RowHTML  string `json:"row_html"`
}

// handleEventStream streams a run's events over SSE. It works like this:
//
//  1. Snapshot catch-up via s.store.Read — every event with Seq > since
//     is emitted immediately, in order.
//  2. Tail via s.store.Stream, filtering out any event with
//     Seq <= lastSent. This handles both the race between Read and
//     Stream (Stream will re-deliver history) and the in-memory
//     backend's documented history/live interleave
//     (eventlog/memory.go:201-207). The snapshot is our source of
//     ordered history; Stream is only used as a future-events pump.
//     TestLiveStream_LongHistory_ConcurrentAppend is the regression
//     guard — removing the lastSent check below makes it fail.
//
// Closes on: first terminal event, ctx cancel, or closed Stream chan.
// Responds 404 if the run has no events. Query param:
//
//	?since=<uint64>   skip events with Seq <= since (default 0)
//
// URL: GET /run/{runID}/events/stream
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request, runID string) {
	if runID == "" {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)

	ctx := r.Context()

	// Snapshot catch-up. Doing this before setting SSE headers lets us
	// surface a 404 cleanly for unknown runs — once we commit to SSE
	// we can't unring the bell.
	snapshot, err := s.store.Read(ctx, runID)
	if err != nil {
		http.Error(w, "read run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(snapshot) == 0 {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ":ok\n\n"); err != nil {
		return
	}
	flusher.Flush()

	var lastSent uint64
	// Emit the post-since portion of the snapshot.
	for _, ev := range snapshot {
		if ev.Seq <= since {
			continue
		}
		if !s.emitLiveFrame(w, flusher, runID, ev) {
			return
		}
		lastSent = ev.Seq
		if ev.Kind.IsTerminal() {
			s.emitLiveEnd(w, flusher, "terminal")
			return
		}
	}

	// Tail. If the run was already terminal in the snapshot we returned
	// above; getting here means we expect more events. Stream will
	// re-deliver history; we skip everything at or below lastSent.
	ch, err := s.store.Stream(ctx, runID)
	if err != nil {
		s.emitLiveError(w, flusher, "stream: "+err.Error())
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// Channel closed without a terminal — slow-consumer drop
				// or log close. Tell the browser so it can reflect the
				// state rather than silently hanging.
				s.emitLiveEnd(w, flusher, "stream-closed")
				return
			}
			if ev.Seq <= lastSent {
				continue
			}
			if !s.emitLiveFrame(w, flusher, runID, ev) {
				return
			}
			lastSent = ev.Seq
			if ev.Kind.IsTerminal() {
				s.emitLiveEnd(w, flusher, "terminal")
				return
			}
		}
	}
}

// emitLiveFrame writes one event frame. Returns false if the write
// failed — the caller should stop the loop.
func (s *Server) emitLiveFrame(w http.ResponseWriter, flusher http.Flusher, runID string, ev event.Event) bool {
	row := rowFromEvent(runID, ev)
	rowHTML, err := s.tpl.renderPartialString("event_row.html", row)
	if err != nil {
		s.emitLiveError(w, flusher, "render row: "+err.Error())
		return false
	}
	payload, err := json.Marshal(liveFrame{
		Seq:      ev.Seq,
		Kind:     ev.Kind.String(),
		Terminal: ev.Kind.IsTerminal(),
		RowHTML:  rowHTML,
	})
	if err != nil {
		s.emitLiveError(w, flusher, "encode frame: "+err.Error())
		return false
	}
	if _, err := fmt.Fprintf(w, "event: event\ndata: %s\n\n", payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func (s *Server) emitLiveEnd(w http.ResponseWriter, flusher http.Flusher, reason string) {
	_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", quoteJSON(reason))
	flusher.Flush()
}

func (s *Server) emitLiveError(w http.ResponseWriter, flusher http.Flusher, msg string) {
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", quoteJSON(msg))
	flusher.Flush()
}
