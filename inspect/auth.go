package inspect

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

// Authenticator reports whether r is allowed to proceed. It runs
// before every request reaches the mux — page routes, the HTMX
// fragment, the live-tail SSE, static assets, replay endpoints, all
// of it. Returning false causes the server to emit 401 and
// short-circuit.
//
// The signature takes the raw *http.Request so callers can inspect
// headers, cookies, the client cert (r.TLS), or r.RemoteAddr; write
// whatever policy makes sense (bearer, JWT, mTLS, IP allowlist, a
// reverse-proxy-set header, etc.). BearerAuth is the one built-in.
type Authenticator func(r *http.Request) bool

// BearerAuth is an Authenticator matching a constant bearer token in
// the Authorization header ("Bearer <token>"). The comparison uses
// subtle.ConstantTimeCompare so a timing side-channel can't distinguish
// a correct prefix from an incorrect one. An empty token panics — the
// caller is asking for "no auth" and should pass nil to WithAuth
// instead.
func BearerAuth(token string) Authenticator {
	if token == "" {
		panic("inspect: BearerAuth called with empty token (pass nil to WithAuth for no auth)")
	}
	want := []byte(token)
	return func(r *http.Request) bool {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			return false
		}
		got := []byte(strings.TrimPrefix(h, "Bearer "))
		if len(got) != len(want) {
			return false
		}
		return subtle.ConstantTimeCompare(got, want) == 1
	}
}

// csrfCookieName is the double-submit cookie name. Kept short because
// it ships on every request.
const csrfCookieName = "starling_csrf"

// csrfHeaderName is the request header replay.js echoes the cookie
// into. X- prefix is legacy but unambiguous in a browser's dev tools.
const csrfHeaderName = "X-CSRF-Token"

// ensureCSRFCookie plants a fresh CSRF token on responses that don't
// already carry one. Called on every request; the caller is
// responsible for guaranteeing it runs before headers are flushed
// (i.e. before s.mux.ServeHTTP). SameSite=Strict prevents the cookie
// from being sent on cross-site requests at all; double-submit then
// defends against the residual CSRF surface (browser bugs, proxy
// weirdness, old clients). HttpOnly is deliberately off so
// replay.js can read it — that's the whole premise of double-submit.
func (s *Server) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) {
	if c, _ := r.Cookie(csrfCookieName); c != nil && c.Value != "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    newCSRFToken(),
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		// HttpOnly: false — JS needs to read this.
	})
}

// checkCSRF returns true iff the request carries a csrf cookie and a
// matching X-CSRF-Token header. Both must be non-empty; a missing
// cookie (scripted client that skipped the seed GET) fails.
func (s *Server) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	h := r.Header.Get(csrfHeaderName)
	if h == "" {
		return false
	}
	if len(h) != len(c.Value) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h), []byte(c.Value)) == 1
}

// isReplayPOST matches the two state-changing replay endpoints:
//
//	POST /run/{id}/replay
//	POST /run/{id}/replay/{session}/control
//
// Kept as a string predicate rather than a routing table so middleware
// can classify without re-parsing the path. {id} may contain "/" but
// "/replay" only appears in these two routes, so a simple Contains
// check suffices.
func isReplayPOST(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/run/") {
		return false
	}
	return strings.Contains(p, "/replay")
}

// newCSRFToken returns 32 random bytes, base64-url encoded. Panics on
// rand failure, which on every platform we care about means the
// process is already unrecoverable.
func newCSRFToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("inspect: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
