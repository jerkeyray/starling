// Package inspect implements Starling's local web inspector as a
// reusable library. The standalone binary cmd/starling-inspect is a
// thin shim around inspect.New; downstream users who want full
// functionality (including replay-from-UI, M5+) wire inspect.New
// into their own agent binary so the server can construct the user's
// Agent on demand.
//
// inspect.New is read-mostly: it serves an HTTP UI backed by an
// eventlog.EventLog (which must also satisfy eventlog.RunLister).
// It never calls Append. To enforce that at the storage layer too,
// pass an EventLog opened with eventlog.WithReadOnly().
package inspect

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/replay"
)

// uiFS holds every file under ui/ — templates and static assets —
// embedded into the package at compile time so any binary linking
// against this package ships the UI inline. No network dependency,
// no CDN, no separate asset directory at runtime.
//
//go:embed ui
var uiFS embed.FS

// staticFS is the ui/static subtree, exposed under /static/.
func staticFS() fs.FS {
	sub, err := fs.Sub(uiFS, "ui/static")
	if err != nil {
		// Sub only fails on a malformed path; ours is a constant.
		panic(err)
	}
	return sub
}

// Server is the inspector HTTP handler. Construct via New and either
// pass it to http.Server.Handler directly or mount it under a
// reverse proxy. Server is safe for concurrent use.
type Server struct {
	store    eventlog.EventLog
	lister   eventlog.RunLister
	tpl      *templates
	mux      *http.ServeMux
	replayer replay.Factory // nil → view-only; replay routes hidden / 404
	auth     Authenticator  // nil → public (default localhost posture)

	sessMu   sync.Mutex
	sessions map[string]*replaySession
}

// Option customises a Server at construction time. None are exported
// yet; the type exists so the API can grow (WithReplayer, WithLogger,
// …) without a breaking change.
type Option func(*Server)

// New builds a Server backed by store. store must also implement
// eventlog.RunLister (both built-in backends do); New returns an
// error otherwise so a future write-only backend doesn't silently
// produce an inspector that can't list runs.
func New(store eventlog.EventLog, opts ...Option) (*Server, error) {
	if store == nil {
		return nil, errors.New("inspect: store is nil")
	}
	lister, ok := store.(eventlog.RunLister)
	if !ok {
		return nil, errors.New("inspect: store does not implement eventlog.RunLister")
	}
	s := &Server{
		store:    store,
		lister:   lister,
		tpl:      mustParseTemplates(uiFS),
		mux:      http.NewServeMux(),
		sessions: make(map[string]*replaySession),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	if s.replayer != nil {
		go s.gcReplaySessions()
	}
	return s, nil
}

// ReplayEnabled reports whether a Replayer was wired in via
// WithReplayer. UI templates use this to hide the Replay button on
// view-only deployments.
func (s *Server) ReplayEnabled() bool { return s.replayer != nil }

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleRuns)
	s.mux.HandleFunc("/run/", s.dispatchRun)
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS()))))
}

// dispatchRun routes within /run/. runID may contain "/"
// (Namespace + "/" + ULID) so we peel known suffixes from the right.
// Replay routes 404 when no Replayer is configured.
//
//	GET  /run/{id}                            → handleRun
//	GET  /run/{id}/event/{seq}                → handleEventDetail (HTMX)
//	GET  /run/{id}/events/stream              → handleEventStream (SSE)
//	GET  /run/{id}/replay                     → handleReplayPage
//	POST /run/{id}/replay                     → handleReplayStart
//	GET  /run/{id}/replay/{session}/stream    → handleReplayStream
//	POST /run/{id}/replay/{session}/control   → handleReplayControl
func (s *Server) dispatchRun(w http.ResponseWriter, r *http.Request) {
	// r.URL.Path is already percent-decoded by net/http; decoding again
	// would collapse %2F into "/" and let an attacker split a runID
	// (which legitimately contains "/" when Namespace is set) into
	// a runID + session + action triple for dispatchReplaySession.
	rest := r.URL.Path[len("/run/"):]
	if rest == "" {
		http.NotFound(w, r)
		return
	}

	// /run/{id}/events/stream — live-tail SSE. Checked before /event/
	// because "/events/stream" contains no "/event/" substring (note
	// the "s") so ordering is nominal, but keeping the explicit path
	// up top avoids future confusion.
	if strings.HasSuffix(rest, "/events/stream") {
		runID := strings.TrimSuffix(rest, "/events/stream")
		if runID == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleEventStream(w, r, runID)
		return
	}

	// /run/{id}/event/{seq}
	if i := strings.LastIndex(rest, "/event/"); i >= 0 {
		runID := rest[:i]
		seqStr := rest[i+len("/event/"):]
		if runID == "" || seqStr == "" || strings.Contains(seqStr, "/") {
			http.NotFound(w, r)
			return
		}
		s.handleEventDetail(w, r, runID, seqStr)
		return
	}

	// /run/{id}/replay/{session}/{action}
	if i := strings.LastIndex(rest, "/replay/"); i >= 0 {
		if s.replayer == nil {
			http.NotFound(w, r)
			return
		}
		runID := rest[:i]
		tail := rest[i+len("/replay/"):]
		parts := strings.Split(tail, "/")
		if runID == "" || len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		s.dispatchReplaySession(w, r, runID, parts[0], parts[1])
		return
	}

	// /run/{id}/replay
	if strings.HasSuffix(rest, "/replay") {
		if s.replayer == nil {
			http.NotFound(w, r)
			return
		}
		runID := rest[:len(rest)-len("/replay")]
		if runID == "" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleReplayPage(w, r, runID)
		case http.MethodPost:
			s.handleReplayStart(w, r, runID)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// /run/{id}
	s.handleRun(w, r, rest)
}

// ServeHTTP implements http.Handler. Runs auth (if configured) and
// double-submit CSRF (always-on for the two replay POSTs) before the
// mux. Rationale for the single chokepoint: a half-auth posture
// (static public, pages private) invites reflected-file tricks and
// buys no meaningful UX — unauthenticated users should see a bare
// 401, not a broken-CSS page.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil && !s.auth(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="starling-inspect"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.ensureCSRFCookie(w, r)
	// Guard every unsafe method rather than enumerating paths — a new
	// POST route added to the mux is CSRF-protected by default instead
	// of silently bypassing until someone remembers to update the
	// allowlist. Safe methods (GET/HEAD/OPTIONS) don't mutate.
	if isUnsafeMethod(r.Method) && !s.checkCSRF(r) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}
