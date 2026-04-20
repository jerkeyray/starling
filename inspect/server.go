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
	// Single prefix for everything under /run/. The dispatcher peels
	// off the path and decides between the full-page detail view, the
	// HTMX-swappable event-detail fragment, and (when a Replayer is
	// wired) the replay-session endpoints.
	s.mux.HandleFunc("/run/", s.dispatchRun)
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS()))))
}

// dispatchRun routes within the /run/ prefix:
//
//	GET  /run/{id}                                  → handleRun (view)
//	GET  /run/{id}/event/{seq}                      → handleEventDetail (HTMX)
//	POST /run/{id}/replay                           → handleReplayStart  (T43)
//	GET  /run/{id}/replay/{session}/stream          → handleReplayStream (SSE, T43)
//	POST /run/{id}/replay/{session}/control         → handleReplayControl (T43)
//
// Replay routes 404 when no Replayer is configured.
func (s *Server) dispatchRun(w http.ResponseWriter, r *http.Request) {
	rest := r.URL.Path[len("/run/"):]
	switch {
	case strings.Contains(rest, "/event/"):
		s.handleEventDetail(w, r)
	case strings.Contains(rest, "/replay"):
		if s.replayer == nil {
			http.NotFound(w, r)
			return
		}
		s.dispatchReplay(w, r, rest)
	default:
		s.handleRun(w, r)
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}
