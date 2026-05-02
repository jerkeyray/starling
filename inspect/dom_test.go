package inspect

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
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

func TestDOM_RunsPage_Paginates(t *testing.T) {
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	for i := 0; i < 5; i++ {
		seedRunStartedAt(t, log, "run-"+string(rune('a'+i)), time.Unix(0, int64(i+1)))
	}
	srv, err := New(log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	body := getBody(t, hs.URL+"/?per_page=2")
	if !strings.Contains(body, "1-2") || !strings.Contains(body, "of 5") || !strings.Contains(body, "next") {
		t.Fatalf("page 1 did not render expected pagination: %q", body)
	}
	if strings.Contains(body, "run-a") {
		t.Fatalf("page 1 included oldest run unexpectedly")
	}

	body = getBody(t, hs.URL+"/?per_page=2&page=2")
	if !strings.Contains(body, "3-4") || !strings.Contains(body, "previous") || !strings.Contains(body, "next") {
		t.Fatalf("page 2 did not render expected pagination: %q", body)
	}

	body = getBody(t, hs.URL+"/?per_page=2&page=99")
	if !strings.Contains(body, "5-5") || strings.Contains(body, "next</a>") {
		t.Fatalf("out-of-range page did not clamp to final page: %q", body)
	}
}

func TestDOM_RunPage_HasTimelineAndReplayLink(t *testing.T) {
	hs, runID := authServer(t)
	body := getBody(t, hs.URL+"/run/"+runID)
	if !hasNodeWithID(t, body, "timeline") {
		t.Errorf("run page missing #timeline")
	}
}

func TestDOM_DiffOptions_CappedToLatest100(t *testing.T) {
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })
	for i := 0; i < 105; i++ {
		seedRunStartedAt(t, log, fmt.Sprintf("run-%03d", i), time.Unix(0, int64(i+1)))
	}
	srv, err := New(log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	body := getBody(t, hs.URL+"/diff")
	if strings.Contains(body, "run-000") || strings.Contains(body, "run-004") {
		t.Fatalf("diff options include oldest runs despite cap")
	}
	if !strings.Contains(body, "run-104") {
		t.Fatalf("diff options missing latest run")
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

func seedRunStartedAt(t *testing.T, log eventlog.EventLog, runID string, ts time.Time) {
	t.Helper()
	payload, err := event.EncodePayload(event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          runID,
		ProviderID:    "fake",
		ModelID:       "m",
	})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	ev := event.Event{
		RunID:     runID,
		Seq:       1,
		Timestamp: ts.UnixNano(),
		Kind:      event.KindRunStarted,
		Payload:   payload,
	}
	if err := log.Append(context.Background(), runID, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
}
