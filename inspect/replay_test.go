package inspect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/replay"
)

// fakeStreamingAgent is a minimal StreamingAgent for end-to-end
// session tests. Mirrors the one in replay/stream_test.go so this
// suite stays self-contained and doesn't import that test file.
type fakeStreamingAgent struct {
	emitN    int
	finalErr error
}

func (f *fakeStreamingAgent) RunReplay(_ context.Context, _ []event.Event) error {
	return errors.New("not used")
}

func (f *fakeStreamingAgent) RunReplayInto(ctx context.Context, recorded []event.Event, sink eventlog.EventLog) error {
	n := f.emitN
	if n < 0 || n > len(recorded) {
		n = len(recorded)
	}
	for i := 0; i < n; i++ {
		// Pace emissions so a paused session has time to actually
		// observe the pause: without this, all events land in the
		// 16-cap source channel before the SSE handler reads even
		// one, making pause/step indistinguishable from "fast run".
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
		if err := sink.Append(ctx, recorded[i].RunID, recorded[i]); err != nil {
			return err
		}
	}
	return f.finalErr
}

// seedReplayLog appends the same minimal three-event run replay
// stream tests use, so the inspector's read path has something to
// chew on. Lifted from replay/stream_test.go for test-package
// isolation.
func seedReplayLog(t *testing.T, log eventlog.EventLog, runID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixNano()
	type kp struct {
		k event.Kind
		p any
	}
	steps := []kp{
		{event.KindRunStarted, event.RunStarted{
			SchemaVersion: event.SchemaVersion,
			Goal:          "test",
			ProviderID:    "fake",
			APIVersion:    "v1",
			ModelID:       "m",
		}},
		{event.KindTurnStarted, event.TurnStarted{TurnID: "t1"}},
		{event.KindRunCompleted, event.RunCompleted{FinalText: "ok", TurnCount: 1}},
	}
	var prev []byte
	for i, s := range steps {
		var encoded []byte
		switch v := s.p.(type) {
		case event.RunStarted:
			b, err := event.EncodePayload(v)
			if err != nil {
				t.Fatalf("encode %d: %v", i, err)
			}
			encoded = []byte(b)
		case event.TurnStarted:
			b, err := event.EncodePayload(v)
			if err != nil {
				t.Fatalf("encode %d: %v", i, err)
			}
			encoded = []byte(b)
		case event.RunCompleted:
			b, err := event.EncodePayload(v)
			if err != nil {
				t.Fatalf("encode %d: %v", i, err)
			}
			encoded = []byte(b)
		}
		ev := event.Event{
			RunID:     runID,
			Seq:       uint64(i + 1),
			PrevHash:  prev,
			Timestamp: now + int64(i),
			Kind:      s.k,
			Payload:   encoded,
		}
		if err := log.Append(ctx, runID, ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		marshaled, err := event.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		prev = event.Hash(marshaled)
	}
}

// newTestServer wires an inspect.Server over an in-memory log seeded
// with one run, optionally with a Replayer. Returns the server (so
// individual tests can dispatch handlers directly without an httptest
// roundtrip when they want to) plus the seeded runID.
func newTestServer(t *testing.T, fa *fakeStreamingAgent) (*Server, eventlog.EventLog, string) {
	t.Helper()
	log := eventlog.NewInMemory()
	t.Cleanup(func() { log.Close() })
	const runID = "r-test"
	seedReplayLog(t, log, runID)

	var opts []Option
	if fa != nil {
		opts = append(opts, WithReplayer(func(_ context.Context) (replay.Agent, error) {
			return fa, nil
		}))
	}
	srv, err := New(log, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, log, runID
}

func TestReplayEndpoints_HiddenWhenNoReplayer(t *testing.T) {
	srv, _, runID := newTestServer(t, nil)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	client, token := primeCSRF(t, hs)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay", nil)
	req.Header.Set(csrfHeaderName, token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (replay disabled)", resp.StatusCode)
	}
	if srv.ReplayEnabled() {
		t.Errorf("ReplayEnabled = true, want false")
	}
}

func TestReplayStart_ReturnsSessionID(t *testing.T) {
	srv, _, runID := newTestServer(t, &fakeStreamingAgent{emitN: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	client, token := primeCSRF(t, hs)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 200", resp.StatusCode, body)
	}
	var got struct {
		SessionID string `json:"session_id"`
		RunID     string `json:"run_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionID == "" {
		t.Errorf("session_id is empty")
	}
	if got.RunID != runID {
		t.Errorf("run_id = %q, want %q", got.RunID, runID)
	}
}

func TestReplayStart_UnknownRun_404(t *testing.T) {
	srv, _, _ := newTestServer(t, &fakeStreamingAgent{emitN: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	client, token := primeCSRF(t, hs)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/no-such-run/replay", nil)
	req.Header.Set(csrfHeaderName, token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReplayStream_DeliversAllStepsAndEnds(t *testing.T) {
	srv, _, runID := newTestServer(t, &fakeStreamingAgent{emitN: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	sessionID := startSession(t, hs, runID)

	events := readSSE(t, hs.URL+"/run/"+runID+"/replay/"+sessionID+"/stream", 3*time.Second)

	// Expect 3 step events (matches the seeded recording length) and 1 end.
	stepCount := countEvents(events, "step")
	endCount := countEvents(events, "end")
	if stepCount != 3 {
		t.Errorf("step events = %d, want 3", stepCount)
	}
	if endCount != 1 {
		t.Errorf("end events = %d, want 1", endCount)
	}
}

func TestReplayControl_PauseResume(t *testing.T) {
	// emitN=3 with 5ms pacing → ~15ms total. Pause within 5ms
	// gives the session time to backlog.
	srv, _, runID := newTestServer(t, &fakeStreamingAgent{emitN: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	sessionID := startSession(t, hs, runID)

	// Fire pause immediately, then start consuming SSE.
	postControl(t, hs, runID, sessionID, "pause")

	// Drain whatever has already buffered (could be 0–N events).
	beforeResume := readSSEFor(t, hs.URL+"/run/"+runID+"/replay/"+sessionID+"/stream", 200*time.Millisecond)
	pausedSteps := countEvents(beforeResume, "step")

	postControl(t, hs, runID, sessionID, "resume")

	afterResume := readSSE(t, hs.URL+"/run/"+runID+"/replay/"+sessionID+"/stream", 3*time.Second)
	totalSteps := pausedSteps + countEvents(afterResume, "step")

	if totalSteps != 3 {
		t.Errorf("total step events across pause/resume = %d, want 3", totalSteps)
	}
}

func TestReplayControl_UnknownAction_400(t *testing.T) {
	srv, _, runID := newTestServer(t, &fakeStreamingAgent{emitN: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	sessionID := startSession(t, hs, runID)

	client, token := primeCSRF(t, hs)
	body := strings.NewReader(`{"action":"frobnicate"}`)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay/"+sessionID+"/control", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestReplayControl_UnknownSession_404(t *testing.T) {
	srv, _, runID := newTestServer(t, &fakeStreamingAgent{emitN: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	client, token := primeCSRF(t, hs)
	body := strings.NewReader(`{"action":"pause"}`)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay/deadbeef/control", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReplayGC_SweepsIdleSessions(t *testing.T) {
	srv, _, runID := newTestServer(t, &fakeStreamingAgent{emitN: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	sessionID := startSession(t, hs, runID)

	// Force the session's lastUsed deep into the past, then run the
	// sweep and verify it's gone. Avoids waiting on the real
	// 60-second / 15-second timers in tests.
	srv.sessMu.Lock()
	sess := srv.sessions[sessionID]
	srv.sessMu.Unlock()
	if sess == nil {
		t.Fatal("session missing after start")
	}
	sess.lastUsed.Store(time.Now().Add(-2 * sessionGCAfter).UnixNano())

	srv.sweepReplaySessions(time.Now())

	srv.sessMu.Lock()
	_, stillThere := srv.sessions[sessionID]
	srv.sessMu.Unlock()
	if stillThere {
		t.Fatalf("session %s not collected by sweep", sessionID)
	}
}

func TestReplay_SessionMaxRejects(t *testing.T) {
	srv, _, runID := newTestServer(t, &fakeStreamingAgent{emitN: -1})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	for i := 0; i < sessionMax; i++ {
		startSession(t, hs, runID)
	}
	client, token := primeCSRF(t, hs)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay", nil)
	req.Header.Set(csrfHeaderName, token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
}

// TestRoutes_NamespacedRunID exercises every public route with a runID
// that itself contains "/" (Agent.Namespace + "/" + ULID). Regression
// guard for a class of bugs where the dispatcher rejected slashes in
// the runID and made the inspector unusable for multi-tenant deployments.
func TestRoutes_NamespacedRunID(t *testing.T) {
	const runID = "tenant-a/01HZZZZZZZZZZZZZZZZZZZZZZZ"
	log := eventlog.NewInMemory()
	t.Cleanup(func() { log.Close() })
	seedReplayLog(t, log, runID)

	srv, err := New(log, WithReplayer(func(_ context.Context) (replay.Agent, error) {
		return &fakeStreamingAgent{emitN: -1}, nil
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	// Path-encode the slash so the URL is well-formed; the dispatcher
	// PathUnescapes once before route matching.
	encoded := strings.ReplaceAll(runID, "/", "%2F")

	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"detail page", "GET", "/run/" + encoded, http.StatusOK},
		{"event detail", "GET", "/run/" + encoded + "/event/1", http.StatusOK},
		{"replay page", "GET", "/run/" + encoded + "/replay", http.StatusOK},
		{"replay start", "POST", "/run/" + encoded + "/replay", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := http.DefaultClient
			req, _ := http.NewRequest(tc.method, hs.URL+tc.path, nil)
			if tc.method == http.MethodPost {
				var token string
				client, token = primeCSRF(t, hs)
				req.Header.Set(csrfHeaderName, token)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
			}
		})
	}
}

func startSession(t *testing.T, hs *httptest.Server, runID string) string {
	t.Helper()
	client, token := primeCSRF(t, hs)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("start status = %d (%s)", resp.StatusCode, body)
	}
	var got struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	return got.SessionID
}

func postControl(t *testing.T, hs *httptest.Server, runID, sessionID, action string) {
	t.Helper()
	client, token := primeCSRF(t, hs)
	body := strings.NewReader(fmt.Sprintf(`{"action":%q}`, action))
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay/"+sessionID+"/control", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST control %s: %v", action, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("control %s status = %d", action, resp.StatusCode)
	}
}

// primeCSRF returns a client with a cookie jar already carrying the
// starling_csrf cookie (planted by a GET to /), plus the cookie's
// value for echoing into X-CSRF-Token. Every test that POSTs to a
// replay endpoint routes through this to satisfy the always-on
// double-submit middleware.
func primeCSRF(t *testing.T, hs *httptest.Server) (*http.Client, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}
	resp, err := client.Get(hs.URL + "/")
	if err != nil {
		t.Fatalf("prime GET: %v", err)
	}
	resp.Body.Close()
	u, _ := url.Parse(hs.URL)
	for _, c := range jar.Cookies(u) {
		if c.Name == csrfCookieName {
			return client, c.Value
		}
	}
	t.Fatalf("no csrf cookie planted on prime GET")
	return nil, ""
}

// sseEvent is one parsed SSE frame.
type sseEvent struct {
	Event string
	Data  string
}

// readSSE reads from the SSE endpoint until an "end" event arrives
// or the timeout fires. Returns every parsed event.
func readSSE(t *testing.T, url string, timeout time.Duration) []sseEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	return parseSSE(t, resp.Body, func(e sseEvent) bool { return e.Event == "end" })
}

// readSSEFor reads from the SSE endpoint for exactly d, regardless
// of whether an "end" event arrives. Used to observe paused-state
// behaviour where the stream stays open.
func readSSEFor(t *testing.T, url string, d time.Duration) []sseEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Expected: ctx deadline closes the body. Distinguish.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	return parseSSE(t, resp.Body, func(_ sseEvent) bool { return false })
}

// parseSSE is a tiny SSE framer. Splits on blank-line boundaries;
// recognises "event:" and "data:" prefixes. stopAt is called per
// parsed event; returns true to stop reading.
func parseSSE(t *testing.T, r io.Reader, stopAt func(sseEvent) bool) []sseEvent {
	t.Helper()
	br := bufio.NewReader(r)
	var out []sseEvent
	var cur sseEvent
	var buf bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			// Reading a closed-by-deadline body returns a transport
			// error; treat it as end-of-stream rather than a fatal.
			break
		}
		trim := strings.TrimRight(line, "\r\n")
		if trim == "" {
			if cur.Event != "" || cur.Data != "" {
				out = append(out, cur)
				if stopAt(cur) {
					return out
				}
				cur = sseEvent{}
				buf.Reset()
			}
			if err == io.EOF {
				return out
			}
			continue
		}
		switch {
		case strings.HasPrefix(trim, "event:"):
			cur.Event = strings.TrimSpace(strings.TrimPrefix(trim, "event:"))
		case strings.HasPrefix(trim, "data:"):
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(strings.TrimSpace(strings.TrimPrefix(trim, "data:")))
			cur.Data = buf.String()
		}
		if err == io.EOF {
			return out
		}
	}
	return out
}

func countEvents(events []sseEvent, name string) int {
	n := 0
	for _, e := range events {
		if e.Event == name {
			n++
		}
	}
	return n
}
