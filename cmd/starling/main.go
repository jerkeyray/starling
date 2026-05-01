// Command starling is the runtime CLI for Starling event logs. It
// bundles the validation, export, inspector, and replay subcommands
// so operators can poke at a log without writing Go code.
//
// Subcommands:
//
//	starling validate <db> [<runID>]   # hash-chain + Merkle check
//	starling export   <db> <runID>     # dump events as NDJSON for jq
//	starling inspect  [flags] <db>     # local web inspector (read-only)
//	starling replay   <db> <runID>     # headless replay (dual-mode only)
//
// The stock `starling` binary serves `replay` in error-only mode —
// re-executing a run requires the user's agent factory, which must
// be linked into the binary. Users who need replay-from-CLI should
// build a dual-mode wrapper around starling.ReplayCommand.
//
// All subcommands open the event log read-only.
package main

import (
	"fmt"
	"os"

	starling "github.com/jerkeyray/starling"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	var err error
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "-v", "--version", "version":
		fmt.Println("starling", starling.Version)
		return
	case "validate":
		err = starling.ValidateCommand().Run(args)
	case "export":
		err = starling.ExportCommand().Run(args)
	case "inspect":
		c := starling.InspectCommand(nil) // view-only: stock binary has no factory
		c.Name = "inspect"
		err = c.Run(args)
	case "replay":
		err = starling.ReplayCommand(nil).Run(args) // nil → clean error
	case "migrate":
		err = starling.MigrateCommand().Run(args)
	case "schema-version":
		err = starling.SchemaVersionCommand().Run(args)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "starling: unknown subcommand %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "starling:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "Usage: starling <command> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  validate   Validate the hash chain of one run (or every run).")
	fmt.Fprintln(w, "  export     Dump one run as NDJSON (one event per line).")
	fmt.Fprintln(w, "  inspect    Serve the local web inspector (read-only).")
	fmt.Fprintln(w, "  replay     Headless replay of one run. Requires a dual-mode binary.")
	fmt.Fprintln(w, "  migrate    Apply pending schema migrations to a SQLite event log.")
	fmt.Fprintln(w, "  schema-version  Print the schema version of a SQLite event log.")
	fmt.Fprintln(w, "  version         Print this binary's Starling version.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'starling <command> -h' for per-command flags.")
}
