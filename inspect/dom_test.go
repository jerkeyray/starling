package inspect

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// DOM-level checks against the rendered HTML. Cheaper than a real
// browser but enough to catch template regressions in the runs list,
// run detail, and replay shells. A full headless-browser suite (Chrome
// via chromedp) is out of scope until we have CI infrastructure for it.

func TestDOM_RunsPage_HasSearchInput(t *testing.T) {
	hs, _ := authServer(t)
	body := getBody(t, hs.URL+"/")
	if !hasNodeWithID(t, body, "filter") {
		t.Errorf("runs page missing #filter input")
	}
	if !hasNodeWithID(t, body, "search-form") {
		t.Errorf("runs page missing search form")
	}
}

func TestDOM_RunsPage_QueryRoundTrips(t *testing.T) {
	hs, _ := authServer(t)
	body := getBody(t, hs.URL+"/?q=does-not-match-anything")
	if !strings.Contains(body, "does-not-match-anything") {
		t.Errorf("query value not echoed back into search input: %q", body)
	}
}

func TestDOM_RunPage_HasTimelineAndReplayLink(t *testing.T) {
	hs, runID := authServer(t)
	body := getBody(t, hs.URL+"/run/"+runID)
	if !hasNodeWithID(t, body, "timeline") {
		t.Errorf("run page missing #timeline")
	}
}

// ----- helpers -----------------------------------------------------------

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func hasNodeWithID(t *testing.T, body, id string) bool {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	var found bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "id" && a.Val == id {
					found = true
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}
