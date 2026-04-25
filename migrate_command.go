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

// MigrateCommand returns a CLI-style entrypoint for `starling migrate`.
func MigrateCommand() *MigrateCmd {
	return &MigrateCmd{Name: "migrate", Output: os.Stdout}
}

// MigrateCmd is the handle returned by MigrateCommand.
type MigrateCmd struct {
	Name   string
	Output io.Writer
}

// Run applies pending migrations to the SQLite event log at args[0].
// Flags:
//
//	-dry-run   report pending versions without applying any DDL.
func (c *MigrateCmd) Run(args []string) error {
	if c.Name == "" {
		c.Name = "migrate"
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(c.Output)
	dryRun := fs.Bool("dry-run", false, "report pending migrations without applying")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [-dry-run] <db>\n\n", c.Name)
		fmt.Fprintln(fs.Output(), "Apply pending event-log schema migrations.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected <db>")
	}
	store, err := eventlog.NewSQLite(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("open log %q: %w", fs.Arg(0), err)
	}
	defer store.Close()

	var opts []eventlog.MigrateOption
	if *dryRun {
		opts = append(opts, eventlog.WithDryRun())
	}
	report, err := eventlog.Migrate(context.Background(), store, opts...)
	if err != nil {
		return err
	}
	prefix := ""
	if report.DryRun {
		prefix = "(dry run) "
	}
	if len(report.Applied) == 0 {
		fmt.Fprintf(c.Output, "%s%s schema already at v%d\n", prefix, report.Backend, report.To)
		return nil
	}
	fmt.Fprintf(c.Output, "%s%s migrated %d → %d (applied: %v)\n",
		prefix, report.Backend, report.From, report.To, report.Applied)
	return nil
}

// SchemaVersionCommand returns a CLI-style entrypoint for
// `starling schema-version`.
func SchemaVersionCommand() *SchemaVersionCmd {
	return &SchemaVersionCmd{Name: "schema-version", Output: os.Stdout}
}

// SchemaVersionCmd is the handle returned by SchemaVersionCommand.
type SchemaVersionCmd struct {
	Name   string
	Output io.Writer
}

// Run prints the current schema version of the SQLite event log at
// args[0].
func (c *SchemaVersionCmd) Run(args []string) error {
	if c.Name == "" {
		c.Name = "schema-version"
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(c.Output)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s <db>\n\n", c.Name)
		fmt.Fprintln(fs.Output(), "Print the schema version of an event log.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected <db>")
	}
	store, err := eventlog.NewSQLite(fs.Arg(0), eventlog.WithReadOnly())
	if err != nil {
		return fmt.Errorf("open log %q: %w", fs.Arg(0), err)
	}
	defer store.Close()

	v, err := eventlog.SchemaVersion(context.Background(), store)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Output, "%d\n", v)
	return nil
}
