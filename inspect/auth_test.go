package inspect

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// authServer wires a seeded in-memory test server and mounts it under
// httptest. Factored because every auth/CSRF test needs the same
// setup and varies only in options.
func authServer(t *testing.T, opts ...Option) (*httptest.Server, string) {
	t.Helper()
	srv, _, runID := newTestServer(t, nil)
	// Re-apply opts on top of the base server. newTestServer doesn't
	// take options through, and we don't want to thread a new
	// parameter for one caller — cheat by mutating directly, which
	// is fine because Option funcs mutate *Server anyway.
	for _, opt := range opts {
		opt(srv)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs, runID
}

// TestAuth_NoAuth_200 confirms the default posture — no WithAuth
// option — keeps every route reachable. Regression guard against a
// future middleware accidentally denying by default.
func TestAuth_NoAuth_200(t *testing.T) {
	hs, _ := authServer(t)

	resp, err := http.Get(hs.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestAuth_MissingBearer_401 checks that an auth-gated server rejects
// a request with no Authorization header, and that the
// WWW-Authenticate response header names the Bearer scheme so a
// well-behaved client can react.
func TestAuth_MissingBearer_401(t *testing.T) {
	hs, _ := authServer(t, WithAuth(BearerAuth("s3cret")))

	resp, err := http.Get(hs.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if wa := resp.Header.Get("WWW-Authenticate"); !strings.Contains(wa, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want to contain Bearer", wa)
	}
}

// TestAuth_WrongBearer_401 guards against a constant-time compare
// regression: a token that's the wrong length or wrong content must
// be rejected the same way.
func TestAuth_WrongBearer_401(t *testing.T) {
	hs, _ := authServer(t, WithAuth(BearerAuth("s3cret")))

	for _, tok := range []string{"nope", "s3cre", "s3cretX", ""} {
		req, _ := http.NewRequest(http.MethodGet, hs.URL+"/", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %q: %v", tok, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("tok=%q status = %d, want 401", tok, resp.StatusCode)
		}
	}
}

// TestAuth_GoodBearer_200 is the happy path: correct token, normal
// 200 response.
func TestAuth_GoodBearer_200(t *testing.T) {
	hs, _ := authServer(t, WithAuth(BearerAuth("s3cret")))

	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestAuth_GatesSSE verifies auth runs ahead of the custom
// /run/{id}/events/stream dispatcher — once we commit to SSE we
// can't unring the 401, and a leaked stream is as bad as a leaked
// page.
func TestAuth_GatesSSE(t *testing.T) {
	hs, runID := authServer(t, WithAuth(BearerAuth("s3cret")))

	resp, err := http.Get(hs.URL + "/run/" + runID + "/events/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, SSE headers leaked past auth", ct)
	}
}

// TestBearerAuth_PanicsOnEmpty is an API-misuse guard: empty tokens
// are always wrong (they'd accept any "Bearer " prefix). Panic is
// preferable to a silently broken server.
func TestBearerAuth_PanicsOnEmpty(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("BearerAuth(\"\") did not panic")
		}
	}()
	_ = BearerAuth("")
}

// fakeFactoryServer wires a server with a replay.Factory so the
// replay POST endpoints are live (not 404). CSRF tests only care
// that we get far enough into the handler to see its real response
// code; the factory never actually runs to completion.
func csrfServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv, _, runID := newTestServer(t, &fakeStreamingAgent{emitN: 0})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs, runID
}

// TestCSRF_POSTWithoutToken_403 confirms the default posture: POSTs
// to replay without a token are rejected with 403 (not 401 — CSRF
// is a distinct failure from auth).
func TestCSRF_POSTWithoutToken_403(t *testing.T) {
	hs, runID := csrfServer(t)

	resp, err := http.Post(hs.URL+"/run/"+runID+"/replay", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestCSRF_POSTMatchingToken_OK exercises the double-submit happy
// path: a GET to plant the cookie, then a POST echoing the cookie
// into X-CSRF-Token. The POST must make it past middleware; the
// handler's own response code is what we inspect.
func TestCSRF_POSTMatchingToken_OK(t *testing.T) {
	hs, runID := csrfServer(t)
	jar := newCookieJar(t)
	client := &http.Client{Jar: jar}

	// Seed the cookie.
	resp, err := client.Get(hs.URL + "/run/" + runID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	token := readCSRFCookie(t, jar, hs.URL)
	if token == "" {
		t.Fatal("no csrf cookie planted on the GET")
	}

	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay", nil)
	req.Header.Set("X-CSRF-Token", token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	// The real handler may return 200 (session created) or any
	// non-403; what we're guarding against is the CSRF middleware
	// rejecting a matching request.
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("status = 403, middleware rejected a matching token")
	}
}

// TestCSRF_WrongToken_403 mismatches cookie vs header. The cookie is
// seeded via the jar; the header is wrong.
func TestCSRF_WrongToken_403(t *testing.T) {
	hs, runID := csrfServer(t)
	jar := newCookieJar(t)
	client := &http.Client{Jar: jar}

	resp, err := client.Get(hs.URL + "/run/" + runID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/run/"+runID+"/replay", nil)
	req.Header.Set("X-CSRF-Token", "not-the-right-token")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestCSRF_GETExempt verifies GETs are never rejected on CSRF
// grounds — the page that plants the cookie has to render.
func TestCSRF_GETExempt(t *testing.T) {
	hs, runID := csrfServer(t)

	// No cookie, no header, GET on the replay page.
	resp, err := http.Get(hs.URL + "/run/" + runID + "/replay")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("GET rejected with 403; CSRF must be POST-only")
	}
	// Cookie should be planted by the GET.
	if cs := resp.Cookies(); !hasCSRFCookie(cs) {
		t.Errorf("no csrf cookie set on GET response")
	}
}

// TestCSRF_UnsafeMethodGuarded guards the CSRF posture: every
// unsafe method (POST/PUT/PATCH/DELETE) on every path requires a
// valid token, so a future mutating route added to the mux is
// protected by default instead of silently bypassing. An
// unauthenticated POST to a non-existent path must 403 on CSRF,
// not 404 on the mux — the CSRF check runs before routing.
func TestCSRF_UnsafeMethodGuarded(t *testing.T) {
	hs, _ := csrfServer(t)

	resp, err := http.Post(hs.URL+"/nonexistent", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without CSRF = %d, want 403", resp.StatusCode)
	}
}

func hasCSRFCookie(cs []*http.Cookie) bool {
	for _, c := range cs {
		if c.Name == csrfCookieName && c.Value != "" {
			return true
		}
	}
	return false
}

func newCookieJar(t *testing.T) *cookiejar.Jar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return jar
}

func readCSRFCookie(t *testing.T, jar *cookiejar.Jar, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	for _, c := range jar.Cookies(u) {
		if c.Name == csrfCookieName {
			return c.Value
		}
	}
	return ""
}
