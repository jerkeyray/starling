package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/jerkeyray/starling/eventlog"
)

// uiFS holds every file under ui/ — templates and static assets —
// embedded into the binary at compile time so a `go install` of this
// command produces a single self-contained executable. No network
// dependency, no CDN, no separate asset directory at runtime.
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

// server bundles everything per-request handlers need: the event log
// (for Read), the run lister (for ListRuns), and the parsed templates.
type server struct {
	store  eventlog.EventLog
	lister eventlog.RunLister
	tpl    *templates
	mux    *http.ServeMux
}

func newServer(store eventlog.EventLog, lister eventlog.RunLister) *server {
	s := &server{
		store:  store,
		lister: lister,
		tpl:    mustParseTemplates(uiFS),
		mux:    http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *server) routes() {
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
func (s *server) dispatchRun(w http.ResponseWriter, r *http.Request) {
	rest := r.URL.Path[len("/run/"):]
	if strings.Contains(rest, "/event/") {
		s.handleEventDetail(w, r)
		return
	}
	s.handleRun(w, r)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}
