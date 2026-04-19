package main

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
)

// templates holds every parsed template under ui/*.html. Page
// templates are wrapped in the shared layout; partial templates
// (HTMX fragments) are parsed standalone.
type templates struct {
	pages    map[string]*template.Template
	partials map[string]*template.Template
}

// mustParseTemplates parses ui/layout.html plus every other ui/*.html
// page template. Each page template is associated with the layout so
// {{template "content" .}} inside the layout dispatches to the page's
// content block. Partials (right-pane HTMX swaps) are parsed
// standalone since they ship without the topbar / page chrome.
// Panics on parse error — broken templates are a release-blocker,
// not a runtime concern.
func mustParseTemplates(fsys embed.FS) *templates {
	const layoutPath = "ui/layout.html"
	pages := []string{
		"ui/runs.html",
		"ui/run.html",
	}
	partials := []string{
		"ui/event_detail.html",
	}

	pageTpls := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		// Parse layout + page + every partial so pages can reference
		// partials via {{template "name" .}} (e.g. run.html pre-renders
		// the first event-detail panel using event_detail.html).
		paths := append([]string{layoutPath, p}, partials...)
		t, err := template.New("layout.html").ParseFS(fsys, paths...)
		if err != nil {
			panic(fmt.Sprintf("parse %s: %v", p, err))
		}
		pageTpls[basename(p)] = t
	}
	partTpls := make(map[string]*template.Template, len(partials))
	for _, p := range partials {
		t, err := template.New(basename(p)).ParseFS(fsys, p)
		if err != nil {
			panic(fmt.Sprintf("parse partial %s: %v", p, err))
		}
		partTpls[basename(p)] = t
	}
	return &templates{pages: pageTpls, partials: partTpls}
}

// render writes the named page template through the shared layout.
// Sets Content-Type to text/html and the appropriate status code.
func (t *templates) render(w http.ResponseWriter, name string, status int, data any) {
	tpl, ok := t.pages[name]
	if !ok {
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		// Headers already sent, so the best we can do is log on the
		// server side; ResponseWriter has no recovery from this.
		_, _ = io.WriteString(w, "\n<!-- template error: "+err.Error()+" -->\n")
	}
}

// renderPartial writes a standalone fragment template (used for
// HTMX swaps). Unlike render, it does not invoke the layout.
func (t *templates) renderPartial(w http.ResponseWriter, name string, status int, data any) {
	tpl, ok := t.partials[name]
	if !ok {
		http.Error(w, "partial not found: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tpl.ExecuteTemplate(w, name, data); err != nil {
		_, _ = io.WriteString(w, "\n<!-- partial template error: "+err.Error()+" -->\n")
	}
}

func basename(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

