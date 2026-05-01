// Command starling-inspect is a local web inspector for Starling
// event logs. It opens a SQLite event log read-only, serves a self-
// contained web UI on localhost, and (by default) opens the user's
// browser pointed at it.
//
// Read-only by construction: the binary opens its SQLite database
// with eventlog.WithReadOnly() and never imports any code path that
// could call EventLog.Append. Inspector cannot mutate audit logs.
//
// Replay is not wired in this binary — it can't be, because replay
// needs to construct the user's Agent and this binary knows nothing
// about it. Users who want replay-from-UI should build their own
// dual-mode binary around starling.InspectCommand (which accepts a
// replay.Factory). See examples/m1_hello for a working example.
//
// Usage:
//
//	starling-inspect runs.db                # localhost, free port, opens browser
//	starling-inspect --addr=:8080 runs.db   # bind explicit port
//	starling-inspect --no-open  runs.db     # don't open browser (SSH / headless)
package main

import (
	"fmt"
	"os"

	starling "github.com/jerkeyray/starling"
)

func main() {
	args := os.Args[1:]
	for _, a := range args {
		if a == "-v" || a == "--version" || a == "version" {
			fmt.Println("starling-inspect", starling.Version)
			return
		}
	}
	cmd := starling.InspectCommand(nil) // nil factory → view-only
	cmd.Name = "starling-inspect"
	if err := cmd.Run(args); err != nil {
		fmt.Fprintln(os.Stderr, "starling-inspect:", err)
		os.Exit(1)
	}
}
