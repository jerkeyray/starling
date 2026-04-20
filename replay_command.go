package starling

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jerkeyray/starling/replay"
)

// ReplayCommand returns a CLI-style entrypoint for `starling replay`.
// Intended for dual-mode binaries that link their agent factory into
// the same binary that serves the runtime CLI.
//
// Shape:
//
//	func main() {
//	    if len(os.Args) > 1 && os.Args[1] == "replay" {
//	        cmd := starling.ReplayCommand(myAgentFactory)
//	        if err := cmd.Run(os.Args[2:]); err != nil {
//	            log.Fatal(err)
//	        }
//	        return
//	    }
//	    // ... normal agent run ...
//	}
//
// factory may be nil: Run then returns an error explaining the
// dual-mode requirement, so the stock `cmd/starling` binary fails
// cleanly rather than pretending replay is possible without user
// code.
//
// When factory is non-nil it is invoked once per Run to construct a
// replay.Agent configured equivalently to the original run.
func ReplayCommand(factory replay.Factory) *ReplayCmd {
	return &ReplayCmd{
		Factory: factory,
		Name:    "replay",
		Output:  os.Stdout,
	}
}

// ReplayCmd is the handle returned by ReplayCommand.
type ReplayCmd struct {
	// Factory builds the agent that re-executes the recorded run. Nil
	// is valid and makes Run return a dual-mode-guidance error — that
	// path is what the stock `cmd/starling` binary uses.
	Factory replay.Factory
	// Name is used in flag error messages and the usage string.
	Name string
	// Output is where the text divergence report is written. Defaults
	// to os.Stdout.
	Output io.Writer
}

// Run parses args and replays one run. Prints `OK: replay matches
// recorded log` on clean replay; `DIVERGED: <reason>` on
// non-determinism, returning a non-nil error so callers exit
// non-zero.
//
// args shape: <db> <runID>
func (c *ReplayCmd) Run(args []string) error {
	if c.Name == "" {
		c.Name = "replay"
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(c.Output)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s <db> <runID>\n\n", c.Name)
		if c.Factory == nil {
			fmt.Fprintln(fs.Output(), "Replay is not wired in this binary.")
			fmt.Fprintln(fs.Output(), "Build a dual-mode binary that calls starling.ReplayCommand(factory).")
		} else {
			fmt.Fprintln(fs.Output(), "Headless replay of <runID>. Prints OK or DIVERGED; exit 1 on divergence.")
		}
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.Factory == nil {
		return errors.New("replay requires a dual-mode binary; see starling.ReplayCommand godoc")
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return errors.New("expected <db> <runID>")
	}

	store, err := openStore(fs.Arg(0))
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	agent, err := c.Factory(ctx)
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}

	err = replay.Verify(ctx, store, fs.Arg(1), agent)
	switch {
	case err == nil:
		fmt.Fprintln(c.Output, "OK: replay matches recorded log")
		return nil
	case errors.Is(err, replay.ErrNonDeterminism):
		fmt.Fprintf(c.Output, "DIVERGED: %v\n", err)
		return fmt.Errorf("%w: %v", ErrNonDeterminism, err)
	default:
		return err
	}
}
