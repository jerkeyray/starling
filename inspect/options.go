package inspect

import "github.com/jerkeyray/starling/replay"

// WithReplayer wires a replay.Factory into the Server so the
// /run/{id}/replay endpoints (and the "Replay" button in the UI) are
// active. Without this option the inspector is view-only: the replay
// routes return 404 and the button is hidden.
//
// The factory is invoked once per replay session — typically on a POST
// to /run/{id}/replay — to construct the *starling.Agent that will
// re-execute the run. Downstream binaries that ship their own agent
// pass a closure capturing their provider config / tool registry /
// namespace; the inspector itself has no opinion on how the agent is
// built.
func WithReplayer(factory replay.Factory) Option {
	return func(s *Server) {
		s.replayer = factory
	}
}

// WithAuth installs an authentication middleware that runs before
// every request reaches the mux. Passing nil is a no-op — the server
// stays public, matching the default localhost-developer posture. See
// Authenticator and BearerAuth for the shape and the one built-in
// helper.
//
// The middleware gates page routes, the HTMX event-detail fragment,
// the live-tail SSE, static assets, and the replay endpoints alike.
// Unauthenticated requests receive 401 with
// WWW-Authenticate: Bearer realm="starling-inspect".
func WithAuth(fn Authenticator) Option {
	return func(s *Server) {
		s.auth = fn
	}
}
