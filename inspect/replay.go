// Replay session machinery. A session drives one replay.Stream run,
// applies pause/step/resume/restart controls, and forwards ReplaySteps
// to whichever SSE consumer is subscribed. Sessions outlive their SSE
// connections and are GC'd after sessionGCAfter idle. State is
// goroutine-local; control + output cross goroutines via channels.

package inspect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/replay"
)

const (
	// sessionGCAfter is how long a session may sit idle (no live SSE
	// subscriber, no recent control POST) before the janitor cancels
	// it. 60s is long enough that a reload survives, short enough that
	// abandoned tabs don't leak forever.
	sessionGCAfter = 60 * time.Second

	// sessionGCInterval is how often the janitor wakes up to sweep.
	sessionGCInterval = 15 * time.Second

	// sessionMax caps total live sessions to prevent runaway memory
	// in the (unlikely) event of a buggy client looping POST /replay.
	// The plan calls out an LRU evict on overflow; we go simpler and
	// reject at the door.
	sessionMax = 16

	// stepBuffer is the size of the session's outbound channel. Small:
	// the agent loop produces sub-millisecond steps so any backlog
	// here means the SSE consumer is idle, not slow.
	stepBuffer = 32
)

// replayCommand is one of: "pause", "resume", "step", "restart".
type replayCommand string

const (
	cmdPause   replayCommand = "pause"
	cmdResume  replayCommand = "resume"
	cmdStep    replayCommand = "step"
	cmdRestart replayCommand = "restart"
)

// replaySession owns one in-flight replay run. The run goroutine is
// the sole owner of replay-loop state (paused, credits, ...); other
// goroutines interact only via the channels below.
type replaySession struct {
	id    string
	runID string

	// startPaused seeds each runOnce attempt (including restarts) with
	// paused=true so short replays don't blow past the user before
	// step/resume can be clicked. Set via ?paused=1 on the start POST.
	startPaused bool

	out     chan sessionFrame  // goroutine → SSE consumer
	control chan replayCommand // SSE consumer → goroutine

	// lastUsed gates GC. Updated atomically by every endpoint that
	// touches the session (stream subscribe, control POST). Stored as
	// unix nanos for lock-free atomic.LoadInt64.
	lastUsed atomic.Int64

	cancel context.CancelFunc
	done   chan struct{} // closed when the run goroutine exits
}

// sessionFrame is the unit pushed from the run goroutine to the SSE
// consumer. Either Step is meaningful (a streamed replay step), or
// EndReason is set (the underlying replay.Stream finished or errored
// — SSE will emit an "end" event and close).
type sessionFrame struct {
	Step      replay.ReplayStep
	HasStep   bool
	EndReason string // empty unless terminal
}

func (s *replaySession) touch() {
	s.lastUsed.Store(time.Now().UnixNano())
}

// dispatchReplaySession routes the per-session replay endpoints.
// Called by the top-level dispatcher with runID, sessionID, and action
// already isolated from the URL path. Pre-conditions: s.replayer != nil,
// runID != "", and the path was matched as /run/{id}/replay/{session}/{action}.
func (s *Server) dispatchReplaySession(w http.ResponseWriter, r *http.Request, runID, sessionID, action string) {
	switch action {
	case "stream":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleReplayStream(w, r, runID, sessionID)
	case "control":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleReplayControl(w, r, runID, sessionID)
	default:
		http.NotFound(w, r)
	}
}

// handleReplayPage renders the full-page replay UI (two-column
// timeline + control bar). The page itself is static; all dynamic
// behaviour is driven client-side via EventSource on the SSE stream
// endpoint. The handler does not start a session — the page does
// that on load via fetch(POST /replay) — so a refresh always lands
// on a fresh session and the previous one is GC'd within 60s.
func (s *Server) handleReplayPage(w http.ResponseWriter, r *http.Request, runID string) {
	events, err := s.store.Read(r.Context(), runID)
	if err != nil {
		http.Error(w, "read run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(events) == 0 {
		http.NotFound(w, r)
		return
	}
	s.tpl.render(w, "replay.html", http.StatusOK, map[string]any{
		"Title":      "Replay " + runID,
		"RunID":      runID,
		"EventCount": len(events),
	})
}

// handleReplayStart constructs a new session and returns its id as
// JSON. The session is added to the map atomically so a follow-up
// /stream request can find it immediately. Caps at sessionMax to
// prevent runaway memory.
func (s *Server) handleReplayStart(w http.ResponseWriter, r *http.Request, runID string) {
	// Verify the run actually exists before spinning up a session.
	// A session with no log to replay against would just no-op.
	events, err := s.store.Read(r.Context(), runID)
	if err != nil {
		http.Error(w, "read run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(events) == 0 {
		http.NotFound(w, r)
		return
	}

	s.sessMu.Lock()
	if len(s.sessions) >= sessionMax {
		s.sessMu.Unlock()
		http.Error(w, "too many active replay sessions; close some tabs and retry", http.StatusTooManyRequests)
		return
	}
	id, err := newSessionID()
	if err != nil {
		s.sessMu.Unlock()
		http.Error(w, "session id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess := &replaySession{
		id:          id,
		runID:       runID,
		startPaused: r.URL.Query().Get("paused") == "1",
		out:         make(chan sessionFrame, stepBuffer),
		control:     make(chan replayCommand, 4),
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	sess.touch()
	s.sessions[id] = sess
	s.sessMu.Unlock()

	go sess.run(ctx, s.replayer, s.store)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"session_id": id,
		"run_id":     runID,
	})
}

// handleReplayStream is the SSE endpoint. Holds the connection open
// until the session ends, ctx cancels, or the client disconnects.
// Each ReplayStep becomes one SSE "step" event; on session end an
// "end" event is sent and the connection closes.
func (s *Server) handleReplayStream(w http.ResponseWriter, r *http.Request, runID, sessionID string) {
	sess := s.lookupSession(runID, sessionID)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Defeat nginx-style proxy buffering. Doesn't help against every
	// corporate proxy, but covers the common case.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	for {
		sess.touch()
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-sess.out:
			if !ok {
				// Session goroutine exited and closed out. Emit a
				// final "end" event with whatever reason was set on
				// the closing frame (if any) and bail.
				_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", quoteJSON("session ended"))
				flusher.Flush()
				return
			}
			if frame.HasStep {
				payload, err := json.Marshal(frame.Step)
				if err != nil {
					// Malformed payload is a bug, not a client problem;
					// surface it as an SSE error event so the UI can
					// show something meaningful.
					_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", quoteJSON("encode step: "+err.Error()))
					flusher.Flush()
					continue
				}
				_, _ = fmt.Fprintf(w, "event: step\ndata: %s\n\n", payload)
				flusher.Flush()
			}
			if frame.EndReason != "" {
				_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", quoteJSON(frame.EndReason))
				flusher.Flush()
				return
			}
		}
	}
}

// handleReplayControl validates and forwards a control command to
// the session's run goroutine. Returns 200 on enqueue (not on
// completion — control commands are fire-and-forget from the
// client's perspective; the SSE stream reflects their effect).
func (s *Server) handleReplayControl(w http.ResponseWriter, r *http.Request, runID, sessionID string) {
	sess := s.lookupSession(runID, sessionID)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	cmd := replayCommand(body.Action)
	switch cmd {
	case cmdPause, cmdResume, cmdStep, cmdRestart:
	default:
		http.Error(w, "unknown action: "+body.Action, http.StatusBadRequest)
		return
	}
	sess.touch()
	select {
	case sess.control <- cmd:
	default:
		// Control queue full — should never happen in practice (cap 4,
		// commands are tiny), but better to drop loudly than block.
		http.Error(w, "control queue full; retry", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) lookupSession(runID, sessionID string) *replaySession {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess := s.sessions[sessionID]
	if sess == nil || sess.runID != runID {
		return nil
	}
	return sess
}

// gcReplaySessions sweeps every sessionGCInterval and cancels any
// session whose lastUsed is older than sessionGCAfter. Started from
// New only when a Replayer is wired; runs for the Server's lifetime.
func (s *Server) gcReplaySessions() {
	ticker := time.NewTicker(sessionGCInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.sweepReplaySessions(time.Now())
	}
}

// sweepReplaySessions is the testable body of gcReplaySessions.
// Cancels every session idle past sessionGCAfter relative to now.
func (s *Server) sweepReplaySessions(now time.Time) {
	cutoff := now.Add(-sessionGCAfter).UnixNano()
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	for id, sess := range s.sessions {
		if sess.lastUsed.Load() < cutoff {
			sess.cancel()
			delete(s.sessions, id)
		}
	}
}

// run drives the underlying replay.Stream and applies control. Owns
// every piece of session state and is the only goroutine that writes
// to s.out / reads from s.control.
//
// The "restart" command tears down the current replay.Stream and
// kicks off a fresh one without changing the session id — important
// because the SSE subscriber is bound to the id and shouldn't have
// to reconnect to see a restart.
func (s *replaySession) run(ctx context.Context, factory replay.Factory, log eventlog.EventLog) {
	defer close(s.done)
	defer close(s.out)

	for {
		ended, restart := s.runOnce(ctx, factory, log)
		if !restart {
			if ended != "" {
				select {
				case s.out <- sessionFrame{EndReason: ended}:
				case <-ctx.Done():
				}
			}
			return
		}
		// Loop = restart: build a fresh replay.Stream.
	}
}

// runOnce drives one replay.Stream attempt. Returns (endReason,
// restart). On restart, runOnce returns immediately and the outer
// run() loop builds a new stream. On natural end (terminal or
// divergence) endReason describes it.
func (s *replaySession) runOnce(ctx context.Context, factory replay.Factory, log eventlog.EventLog) (endReason string, restart bool) {
	src, err := replay.Stream(ctx, factory, log, s.runID)
	if err != nil {
		return "stream: " + err.Error(), false
	}

	// Per-attempt control state. Resets on restart so a paused
	// previous attempt doesn't carry over (except for the session-level
	// startPaused opt-in, which is meant to apply to every attempt).
	paused := s.startPaused
	credits := 0 // step commands credited while paused

	for {
		// Decide whether we can pull from src this iteration.
		canPull := !paused || credits > 0
		if canPull {
			select {
			case <-ctx.Done():
				return "context cancelled", false
			case cmd := <-s.control:
				switch cmd {
				case cmdPause:
					paused = true
				case cmdResume:
					paused = false
					credits = 0
				case cmdStep:
					if paused {
						credits++
					}
				case cmdRestart:
					return "", true
				}
			case ev, ok := <-src:
				if !ok {
					return "replay completed", false
				}
				select {
				case s.out <- sessionFrame{Step: ev, HasStep: true}:
				case <-ctx.Done():
					return "context cancelled", false
				}
				if paused && credits > 0 {
					credits--
				}
			}
		} else {
			// Paused with no credits — only listen for control or
			// cancellation. Source events back up in the channel
			// (cap 16) which backpressures the underlying agent.
			select {
			case <-ctx.Done():
				return "context cancelled", false
			case cmd := <-s.control:
				switch cmd {
				case cmdPause:
					// already paused; no-op
				case cmdResume:
					paused = false
					credits = 0
				case cmdStep:
					credits++
				case cmdRestart:
					return "", true
				}
			}
		}
	}
}

// newSessionID returns 16 random bytes hex-encoded (32 chars). Crypto
// strength is overkill for in-process IDs but rand.Read is the
// stdlib's only entropy source and the alternative (math/rand seeded)
// is a worse footgun.
func newSessionID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// quoteJSON is fmt.Sprintf("%q", s) with no risk of breaking SSE
// framing (quotes + escaping handled). Cheaper than json.Marshal for
// a single string and the tiny diagnostic strings we put in "end"
// events don't justify the allocation.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
