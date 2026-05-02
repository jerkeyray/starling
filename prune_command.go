package starling

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jerkeyray/starling/eventlog"
)

// PruneCommand returns a CLI-style entrypoint for `starling prune`.
// It deletes whole runs that are older than a retention cutoff. The
// command is dry-run unless --confirm is passed.
func PruneCommand() *PruneCmd {
	return &PruneCmd{Name: "prune", Output: os.Stdout}
}

// PruneCmd is the handle returned by PruneCommand.
type PruneCmd struct {
	// Name is used in flag error messages and the usage string.
	Name string
	// Output is where the retention report is written. Defaults to
	// os.Stdout.
	Output io.Writer
}

func (c *PruneCmd) Run(args []string) error {
	if c.Name == "" {
		c.Name = "prune"
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(c.Output)
	var olderThan time.Duration
	var before string
	var status string
	var limit int
	var includeInProgress bool
	var confirm bool
	fs.DurationVar(&olderThan, "older-than", 0, "delete runs started before now minus this duration (for example 720h)")
	fs.StringVar(&before, "before", "", "delete runs started before this RFC3339 timestamp")
	fs.StringVar(&status, "status", "", "optional status: completed, failed, cancelled, or in progress")
	fs.IntVar(&limit, "limit", 0, "maximum runs to prune in this pass")
	fs.BoolVar(&includeInProgress, "include-in-progress", false, "include in-progress runs when --status is empty")
	fs.BoolVar(&confirm, "confirm", false, "actually delete matching runs; without this flag the command is a dry run")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [flags] <db>\n\n", c.Name)
		fmt.Fprintln(fs.Output(), "Prune whole runs from a SQLite event log after an explicit retention cutoff.")
		fmt.Fprintln(fs.Output(), "Without --confirm, prints the matching runs and deletes nothing.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected <db>")
	}
	cutoff, err := pruneCutoff(olderThan, before)
	if err != nil {
		return err
	}

	var store eventlog.EventLog
	if confirm {
		store, err = openWritableStore(fs.Arg(0))
	} else {
		store, err = openStore(fs.Arg(0))
	}
	if err != nil {
		return err
	}
	defer store.Close()

	pruner, ok := store.(eventlog.RunPruner)
	if !ok {
		return fmt.Errorf("log backend does not support pruning")
	}
	report, err := pruner.PruneRuns(context.Background(), eventlog.PruneOptions{
		Before:            cutoff,
		Status:            status,
		IncludeInProgress: includeInProgress,
		Limit:             limit,
		DryRun:            !confirm,
	})
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	action := "would delete"
	if confirm {
		action = "deleted"
	}
	fmt.Fprintf(c.Output, "%s %d runs (%d events) before %s\n",
		action, report.MatchedRuns, report.MatchedEvents, cutoff.UTC().Format(time.RFC3339),
	)
	for _, runID := range report.RunIDs {
		fmt.Fprintln(c.Output, runID)
	}
	return nil
}

func pruneCutoff(olderThan time.Duration, before string) (time.Time, error) {
	if olderThan != 0 && before != "" {
		return time.Time{}, errors.New("use only one of --older-than or --before")
	}
	if olderThan == 0 && before == "" {
		return time.Time{}, errors.New("expected --older-than or --before")
	}
	if olderThan < 0 {
		return time.Time{}, errors.New("--older-than must be positive")
	}
	if olderThan > 0 {
		return time.Now().Add(-olderThan), nil
	}
	t, err := time.Parse(time.RFC3339, before)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse --before: %w", err)
	}
	return t, nil
}
