package starling

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jerkeyray/starling/eventlog"
)

// ValidateCommand returns a CLI-style entrypoint for `starling validate`.
// Runs eventlog.Validate over one run (or every run in the log) and
// prints per-run status. Intended to be invoked from cmd/starling; the
// returned *ValidateCmd is safe to configure further before Run.
func ValidateCommand() *ValidateCmd {
	return &ValidateCmd{Name: "validate", Output: os.Stdout}
}

// ValidateCmd is the handle returned by ValidateCommand.
type ValidateCmd struct {
	// Name is used in flag error messages and the usage string.
	Name string
	// Output is where per-run status lines are written. Defaults to
	// os.Stdout.
	Output io.Writer
}

// Run parses args and validates the requested run(s). Prints one line
// per run: "<runID>\tOK" or "<runID>\tCORRUPT: <reason>". Returns a
// non-nil error on I/O failure or on any validation failure, so the
// caller (cmd/starling) can exit non-zero.
//
// args shape:
//
//	<db>            validate every run in the log
//	<db> <runID>    validate one run
func (c *ValidateCmd) Run(args []string) error {
	if c.Name == "" {
		c.Name = "validate"
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(c.Output)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s <db> [<runID>]\n\n", c.Name)
		fmt.Fprintln(fs.Output(), "Validate the hash chain and Merkle root of one run, or every run in the log.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return errors.New("expected <db> [<runID>]")
	}
	dbPath := fs.Arg(0)

	store, err := openStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()

	if fs.NArg() == 2 {
		runID := fs.Arg(1)
		return c.validateOne(ctx, store, runID)
	}

	lister, ok := store.(eventlog.RunLister)
	if !ok {
		return fmt.Errorf("log backend does not support ListRuns")
	}
	runs, err := lister.ListRuns(ctx)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if len(runs) == 0 {
		fmt.Fprintln(c.Output, "(no runs)")
		return nil
	}
	var fail error
	for _, r := range runs {
		if err := c.validateOne(ctx, store, r.RunID); err != nil {
			// validateOne already printed the line; remember to fail.
			fail = err
		}
	}
	return fail
}

// validateOne validates a single run and writes one status line. Returns
// an error on corruption so callers can propagate an overall failure.
func (c *ValidateCmd) validateOne(ctx context.Context, store eventlog.EventLog, runID string) error {
	evs, err := store.Read(ctx, runID)
	if err != nil {
		fmt.Fprintf(c.Output, "%s\tERROR: %v\n", runID, err)
		return err
	}
	if len(evs) == 0 {
		msg := fmt.Errorf("run %q not found", runID)
		fmt.Fprintf(c.Output, "%s\tERROR: %v\n", runID, msg)
		return msg
	}
	if err := eventlog.Validate(evs); err != nil {
		fmt.Fprintf(c.Output, "%s\tCORRUPT: %v\n", runID, err)
		return err
	}
	fmt.Fprintf(c.Output, "%s\tOK\n", runID)
	return nil
}
