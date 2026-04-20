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
