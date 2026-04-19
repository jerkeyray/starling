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

	"github.com/jerkeyray/starling/eventlog"
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
	store  eventlog.EventLog
	lister eventlog.RunLister
	tpl    *templates
	mux    *http.ServeMux
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
		store:  store,
		lister: lister,
		tpl:    mustParseTemplates(uiFS),
		mux:    http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleRuns)
	// Single prefix for everything under /run/. The dispatcher peels
	// off the path and decides between the full-page detail view and
	// the HTMX-swappable event-detail fragment.
	s.mux.HandleFunc("/run/", s.dispatchRun)
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS()))))
}

// dispatchRun routes /run/{id} → handleRun and
// /run/{id}/event/{seq} → handleEventDetail. Kept as a single mux
// entry so handleRun can still use strings.TrimPrefix on the full
// path without competing patterns.
func (s *Server) dispatchRun(w http.ResponseWriter, r *http.Request) {
	rest := r.URL.Path[len("/run/"):]
	if strings.Contains(rest, "/event/") {
		s.handleEventDetail(w, r)
		return
	}
	s.handleRun(w, r)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}
