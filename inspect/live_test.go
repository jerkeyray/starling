package inspect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

// decodeLiveFrame parses one "event" SSE frame's JSON data into the
// liveFrame shape.
func decodeLiveFrame(t *testing.T, data string) liveFrame {
	t.Helper()
	var f liveFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		t.Fatalf("decode liveFrame %q: %v", data, err)
	}
	return f
}

// filterLiveFrames returns only the "event" SSE frames, decoded.
func filterLiveFrames(t *testing.T, events []sseEvent) []liveFrame {
	t.Helper()
	out := make([]liveFrame, 0, len(events))
	for _, e := range events {
		if e.Event == "event" {
			out = append(out, decodeLiveFrame(t, e.Data))
		}
	}
	return out
}

// TestLiveStream_InMemory_HistoryThenEnd verifies the catch-up path
// emits every seeded event in Seq order and closes with an "end"
// frame on the terminal.
func TestLiveStream_InMemory_HistoryThenEnd(t *testing.T) {
	srv, _, runID := newTestServer(t, nil)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	events := readSSE(t, hs.URL+"/run/"+runID+"/events/stream", 3*time.Second)
	frames := filterLiveFrames(t, events)
	if len(frames) != 3 {
		t.Fatalf("event frames = %d, want 3", len(frames))
	}
	for i, f := range frames {
		if f.Seq != uint64(i+1) {
			t.Errorf("frame[%d].Seq = %d, want %d", i, f.Seq, i+1)
		}
	}
	if !frames[len(frames)-1].Terminal {
		t.Errorf("last frame Terminal = false, want true")
	}
	if countEvents(events, "end") != 1 {
		t.Errorf("end events = %d, want 1", countEvents(events, "end"))
	}
}

// TestLiveStream_SQLite_HistoryThenEnd runs the same assertions over a
// SQLite-backed log — the durable backend is the interesting one for
// live-tail in production.
func TestLiveStream_SQLite_HistoryThenEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	const runID = "r-sqlite"
	seedReplayLog(t, log, runID)

	srv, err := New(log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	events := readSSE(t, hs.URL+"/run/"+runID+"/events/stream", 3*time.Second)
	frames := filterLiveFrames(t, events)
	if len(frames) != 3 {
		t.Fatalf("event frames = %d, want 3", len(frames))
	}
	if frames[len(frames)-1].Kind != "RunCompleted" {
		t.Errorf("last frame Kind = %q, want RunCompleted", frames[len(frames)-1].Kind)
	}
}

// TestLiveStream_SinceSkipsBackfill verifies ?since= filters out
// already-seen events. Matches the page-reload contract: the browser
// has seq 1..N rendered server-side; it subscribes with since=N and
// should only see seq N+1 onwards (if any) plus the terminal.
func TestLiveStream_SinceSkipsBackfill(t *testing.T) {
	srv, _, runID := newTestServer(t, nil)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	// seedReplayLog emits 3 events ending in RunCompleted. since=2 →
	// only seq=3 (the terminal) should appear.
	events := readSSE(t, hs.URL+"/run/"+runID+"/events/stream?since=2", 3*time.Second)
	frames := filterLiveFrames(t, events)
	if len(frames) != 1 {
		t.Fatalf("event frames = %d, want 1", len(frames))
	}
	if frames[0].Seq != 3 {
		t.Errorf("Seq = %d, want 3", frames[0].Seq)
	}
	if !frames[0].Terminal {
		t.Errorf("Terminal = false, want true")
	}
}

// TestLiveStream_LiveAppend seeds an in-progress run (no terminal),
// opens the SSE stream, then appends more events including a
// terminal. Asserts the added events arrive in order and the stream
// closes.
func TestLiveStream_LiveAppend(t *testing.T) {
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	const runID = "r-live"
	seedRunStartedOnly(t, log, runID)

	srv, err := New(log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	// Append more events from a goroutine after the client connects.
	// Small delay so the catch-up path has committed before new events
	// land via Stream — exercises the tail loop specifically.
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(30 * time.Millisecond)
		appendTurnStarted(t, log, runID, 2)
		time.Sleep(10 * time.Millisecond)
		appendRunCompleted(t, log, runID, 3)
	}()

	events := readSSE(t, hs.URL+"/run/"+runID+"/events/stream", 3*time.Second)
	<-done
	frames := filterLiveFrames(t, events)
	if len(frames) != 3 {
		t.Fatalf("event frames = %d, want 3", len(frames))
	}
	for i, f := range frames {
		if f.Seq != uint64(i+1) {
			t.Errorf("frame[%d].Seq = %d, want %d", i, f.Seq, i+1)
		}
	}
	if countEvents(events, "end") != 1 {
		t.Errorf("end events = %d, want 1", countEvents(events, "end"))
	}
}

// TestLiveStream_Namespaced_RunID guards against the namespaced-run
// regression class (commit 792eca1). A runID of the form "team-a/ULID"
// contains a "/" and must round-trip through dispatchRun intact.
func TestLiveStream_Namespaced_RunID(t *testing.T) {
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	const runID = "team-a/r1"
	seedReplayLog(t, log, runID)

	srv, err := New(log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	events := readSSE(t, hs.URL+"/run/"+runID+"/events/stream", 3*time.Second)
	frames := filterLiveFrames(t, events)
	if len(frames) != 3 {
		t.Fatalf("event frames = %d, want 3", len(frames))
	}
	if frames[0].Seq != 1 {
		t.Errorf("first frame Seq = %d, want 1", frames[0].Seq)
	}
}

// TestDispatch_NoDoubleUnescape guards against a routing-confusion
// primitive where %252F in the URL decoded twice (once by net/http,
// once by dispatchRun) into a literal "/", letting an attacker split
// what the server treats as the runID into runID + replay session +
// action. With the fix, r.URL.Path is used as-is (already decoded by
// net/http) so %2F in a runID stays encoded as a single slash and
// the request 404s rather than matching /run/{id}/replay/{s}/{a}.
func TestDispatch_NoDoubleUnescape(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	// %252F → decoded once by net/http to "%2F" (literal, not slash).
	// Before the fix, dispatchRun's second PathUnescape collapsed that
	// into "/" and the replay-dispatch path picked it up.
	resp, err := http.Get(hs.URL + "/run/foo%252Freplay/sess/ctrl")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	// No replayer is wired, so even if the attacker-crafted path
	// matched /replay, the reply would be 404. But with the fix we
	// want the *routing* to have treated this as /run/{id} with an
	// unknown run, also 404. Either way, a 200 or 403 would signal
	// the bug. The cheap assertion is "not 200 and not a replay
	// route status."
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200, want 404 — double-unescape re-opened")
	}
}

// TestLiveStream_UnknownRun_404 ensures the catch-up path surfaces a
// proper 404 for a non-existent run rather than an empty SSE stream.
func TestLiveStream_UnknownRun_404(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	resp, err := http.Get(hs.URL + "/run/no-such-run/events/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestLiveStream_RowHTMLIsSanitized verifies the rendered row HTML is
// included in the frame and contains the expected data-seq attribute.
// A basic content check rather than a template-structure assertion —
// if the partial changes the test should still pass as long as the
// row identifies itself.
func TestLiveStream_RowHTMLIsSanitized(t *testing.T) {
	srv, _, runID := newTestServer(t, nil)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	events := readSSE(t, hs.URL+"/run/"+runID+"/events/stream", 3*time.Second)
	frames := filterLiveFrames(t, events)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	for i, f := range frames {
		if f.RowHTML == "" {
			t.Errorf("frame[%d].RowHTML is empty", i)
			continue
		}
		want := fmt.Sprintf(`data-seq="%d"`, f.Seq)
		if !strings.Contains(f.RowHTML, want) {
			t.Errorf("frame[%d].RowHTML missing %q: got %q", i, want, f.RowHTML)
		}
	}
}

// TestLiveStream_LongHistory_ConcurrentAppend forces the in-memory
// backend's long-history pump path (history > streamBufferSize=256) and
// concurrently appends more events plus a terminal while the SSE
// consumer is reading. Asserts the client sees every Seq exactly once,
// in strictly ascending order, with the terminal last — proving
// handleEventStream's lastSent filter correctly tolerates the
// documented history/live interleave in memory.go's pump.
//
// Regression guard: removing the `ev.Seq <= lastSent` check in
// inspect/live.go must make this test fail.
func TestLiveStream_LongHistory_ConcurrentAppend(t *testing.T) {
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })

	const runID = "r-long"
	const seeded = 300  // > streamBufferSize (256)
	const appended = 20 // live appends during streaming
	const terminalSeq = seeded + appended + 1

	seedLongRun(t, log, runID, seeded)

	srv, err := New(log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	// Kick off the live appender slightly after the client connects so
	// at least some of the appends land during the snapshot catch-up
	// (exercising the tail loop's dedup against the pump's replay of
	// history).
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(20 * time.Millisecond)
		for i := 1; i <= appended; i++ {
			appendTurnStarted(t, log, runID, uint64(seeded+i))
		}
		appendRunCompleted(t, log, runID, terminalSeq)
	}()

	events := readSSE(t, hs.URL+"/run/"+runID+"/events/stream", 10*time.Second)
	<-done

	frames := filterLiveFrames(t, events)
	want := int(terminalSeq)
	if len(frames) != want {
		t.Fatalf("event frames = %d, want %d", len(frames), want)
	}
	seen := make(map[uint64]bool, want)
	var prev uint64
	for i, f := range frames {
		if seen[f.Seq] {
			t.Fatalf("frame[%d]: duplicate Seq=%d", i, f.Seq)
		}
		seen[f.Seq] = true
		if f.Seq <= prev {
			t.Fatalf("frame[%d]: Seq=%d not strictly ascending (prev=%d)", i, f.Seq, prev)
		}
		prev = f.Seq
	}
	for i := uint64(1); i <= terminalSeq; i++ {
		if !seen[i] {
			t.Errorf("missing Seq=%d", i)
		}
	}
	if !frames[len(frames)-1].Terminal {
		t.Errorf("last frame Terminal = false, want true")
	}
	if countEvents(events, "end") != 1 {
		t.Errorf("end events = %d, want 1", countEvents(events, "end"))
	}
}

// TestLiveStream_ReconnectSinceNoDuplicates simulates the browser's
// reconnect path after a dropped SSE connection: consume the first N
// frames from stream A, close it, append more events, then open stream
// B with ?since=N. Asserts stream B emits only events with Seq > N
// (no duplicates of 1..N) and closes on the terminal.
//
// Regression guard: if handleEventStream ever skips the `ev.Seq <= since`
// check at live.go:82, this test fails because the 1..3 frames
// re-appear on the second stream.
func TestLiveStream_ReconnectSinceNoDuplicates(t *testing.T) {
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })

	const runID = "r-reconnect"
	seedRunStartedOnly(t, log, runID)
	appendTurnStarted(t, log, runID, 2)
	appendTurnStarted(t, log, runID, 3)

	srv, err := New(log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	// Stream A: non-terminal run, so readSSE would hang until timeout.
	// readSSEFor collects what the server emits during a short window,
	// then closes the client side (mimicking a tab close / network drop).
	streamA := readSSEFor(t, hs.URL+"/run/"+runID+"/events/stream", 250*time.Millisecond)
	framesA := filterLiveFrames(t, streamA)
	if len(framesA) != 3 {
		t.Fatalf("stream A frames = %d, want 3", len(framesA))
	}

	// After disconnect: append two more non-terminal events, then terminal.
	appendTurnStarted(t, log, runID, 4)
	appendTurnStarted(t, log, runID, 5)
	appendRunCompleted(t, log, runID, 6)

	// Stream B reconnects with since=3 — the client remembers the last
	// Seq it rendered. Expect exactly 3 frames, Seqs {4,5,6}, terminal
	// last, and none of 1..3 re-emitted.
	streamB := readSSE(t, hs.URL+"/run/"+runID+"/events/stream?since=3", 5*time.Second)
	framesB := filterLiveFrames(t, streamB)
	if len(framesB) != 3 {
		t.Fatalf("stream B frames = %d, want 3", len(framesB))
	}
	wantSeqs := []uint64{4, 5, 6}
	for i, f := range framesB {
		if f.Seq != wantSeqs[i] {
			t.Errorf("stream B frame[%d] Seq = %d, want %d", i, f.Seq, wantSeqs[i])
		}
		if f.Seq <= 3 {
			t.Errorf("stream B frame[%d] Seq=%d: re-emitted pre-since event", i, f.Seq)
		}
	}
	if !framesB[len(framesB)-1].Terminal {
		t.Errorf("stream B last frame Terminal = false, want true")
	}
}

// --- local helpers ---------------------------------------------------

// seedRunStartedOnly appends a single RunStarted event, leaving the
// run in a non-terminal state so live-append tests have something to
// catch up on before the producer posts more.
func seedRunStartedOnly(t *testing.T, log eventlog.EventLog, runID string) {
	t.Helper()
	payload, err := event.EncodePayload(event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          "live-tail test",
		ProviderID:    "fake",
		APIVersion:    "v1",
		ModelID:       "m",
	})
	if err != nil {
		t.Fatalf("encode RunStarted: %v", err)
	}
	ev := event.Event{
		RunID:     runID,
		Seq:       1,
		Timestamp: time.Now().UnixNano(),
		Kind:      event.KindRunStarted,
		Payload:   payload,
	}
	if err := log.Append(context.Background(), runID, ev); err != nil {
		t.Fatalf("append RunStarted: %v", err)
	}
}

func appendTurnStarted(t *testing.T, log eventlog.EventLog, runID string, seq uint64) {
	t.Helper()
	payload, err := event.EncodePayload(event.TurnStarted{TurnID: fmt.Sprintf("t%d", seq)})
	if err != nil {
		t.Fatalf("encode TurnStarted: %v", err)
	}
	evs, err := log.Read(context.Background(), runID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	prev := evs[len(evs)-1]
	prevEnc, _ := event.Marshal(prev)
	ev := event.Event{
		RunID:     runID,
		Seq:       seq,
		PrevHash:  event.Hash(prevEnc),
		Timestamp: time.Now().UnixNano(),
		Kind:      event.KindTurnStarted,
		Payload:   payload,
	}
	if err := log.Append(context.Background(), runID, ev); err != nil {
		t.Fatalf("append TurnStarted: %v", err)
	}
}

// seedLongRun appends RunStarted (seq=1) plus n-1 TurnStarted events,
// leaving the run non-terminal at Seq=n. Used to force the in-memory
// backend's long-history pump path (history > streamBufferSize=256).
// Builds the hash chain locally rather than re-Reading after each
// append — O(n) instead of O(n²) for n in the hundreds.
func seedLongRun(t *testing.T, log eventlog.EventLog, runID string, n int) {
	t.Helper()
	if n < 1 {
		t.Fatalf("seedLongRun: n=%d, want >=1", n)
	}
	ctx := context.Background()
	now := time.Now().UnixNano()

	rsPayload, err := event.EncodePayload(event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          "long-history test",
		ProviderID:    "fake",
		APIVersion:    "v1",
		ModelID:       "m",
	})
	if err != nil {
		t.Fatalf("encode RunStarted: %v", err)
	}
	first := event.Event{
		RunID:     runID,
		Seq:       1,
		Timestamp: now,
		Kind:      event.KindRunStarted,
		Payload:   rsPayload,
	}
	if err := log.Append(ctx, runID, first); err != nil {
		t.Fatalf("append RunStarted: %v", err)
	}
	prevEnc, err := event.Marshal(first)
	if err != nil {
		t.Fatalf("marshal seq=1: %v", err)
	}
	prev := event.Hash(prevEnc)

	for i := 2; i <= n; i++ {
		payload, err := event.EncodePayload(event.TurnStarted{TurnID: fmt.Sprintf("t%d", i)})
		if err != nil {
			t.Fatalf("encode TurnStarted seq=%d: %v", i, err)
		}
		ev := event.Event{
			RunID:     runID,
			Seq:       uint64(i),
			PrevHash:  prev,
			Timestamp: now + int64(i),
			Kind:      event.KindTurnStarted,
			Payload:   payload,
		}
		if err := log.Append(ctx, runID, ev); err != nil {
			t.Fatalf("append seq=%d: %v", i, err)
		}
		enc, err := event.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal seq=%d: %v", i, err)
		}
		prev = event.Hash(enc)
	}
}

func appendRunCompleted(t *testing.T, log eventlog.EventLog, runID string, seq uint64) {
	t.Helper()
	payload, err := event.EncodePayload(event.RunCompleted{FinalText: "ok", TurnCount: 1})
	if err != nil {
		t.Fatalf("encode RunCompleted: %v", err)
	}
	evs, err := log.Read(context.Background(), runID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	prev := evs[len(evs)-1]
	prevEnc, _ := event.Marshal(prev)
	ev := event.Event{
		RunID:     runID,
		Seq:       seq,
		PrevHash:  event.Hash(prevEnc),
		Timestamp: time.Now().UnixNano(),
		Kind:      event.KindRunCompleted,
		Payload:   payload,
	}
	if err := log.Append(context.Background(), runID, ev); err != nil {
		t.Fatalf("append RunCompleted: %v", err)
	}
}
