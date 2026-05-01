package starling

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

// DoctorCommand returns a CLI-style entrypoint for `starling doctor`.
//
// Doctor is a quick health check rolled into a single command: it
// reports the binary's Starling version, the schema version of the
// supplied event log (if any), validates the hash chain, and surveys
// well-known provider env vars. It exits 0 on success, 1 if any
// subcheck fails. Useful as the first thing to run when a downstream
// build "isn't working" — it surfaces version skew, schema drift,
// missing API keys, and chain corruption in one place.
//
// Usage:
//
//	starling doctor                    # env-only checks
//	starling doctor <db>               # env + schema/validate against db
func DoctorCommand() *DoctorCmd {
	return &DoctorCmd{Name: "doctor", Output: os.Stdout}
}

// DoctorCmd is the handle returned by DoctorCommand.
type DoctorCmd struct {
	Name   string
	Output io.Writer
}

// Run executes every subcheck and returns nil iff all pass.
func (c *DoctorCmd) Run(args []string) error {
	if c.Name == "" {
		c.Name = "doctor"
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(c.Output)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [<db>]\n\n", c.Name)
		fmt.Fprintln(fs.Output(), "Run a quick health check on the Starling environment and (optionally) a SQLite event log.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	checks := []doctorCheck{
		c.checkVersion(),
		c.checkProviderEnv(),
	}
	if fs.NArg() >= 1 {
		path := fs.Arg(0)
		checks = append(checks,
			c.checkSchemaVersion(path),
			c.checkValidate(path),
		)
	}

	ok := true
	for _, ch := range checks {
		c.printCheck(ch)
		if !ch.ok {
			ok = false
		}
	}
	if !ok {
		return errors.New("doctor: one or more checks failed")
	}
	return nil
}

type doctorCheck struct {
	name string
	ok   bool
	note string
}

func (c *DoctorCmd) printCheck(ch doctorCheck) {
	mark := "✓"
	if !ch.ok {
		mark = "✗"
	}
	fmt.Fprintf(c.Output, "%s %-22s %s\n", mark, ch.name, ch.note)
}

func (c *DoctorCmd) checkVersion() doctorCheck {
	return doctorCheck{
		name: "starling version",
		ok:   true,
		note: Version,
	}
}

// checkProviderEnv surveys whether at least one provider's API key is
// in the environment. Reports each key it finds; passes if any are
// set, since most users only configure one provider.
func (c *DoctorCmd) checkProviderEnv() doctorCheck {
	keys := []string{
		"OPENAI_API_KEY",
		"ANTHROPIC_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_API_KEY",
		"OPENROUTER_API_KEY",
		"AWS_ACCESS_KEY_ID",
	}
	var found []string
	for _, k := range keys {
		if os.Getenv(k) != "" {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return doctorCheck{
			name: "provider api keys",
			ok:   false,
			note: "none of the well-known provider env vars are set (OPENAI_API_KEY, ANTHROPIC_API_KEY, …)",
		}
	}
	note := "found:"
	for _, k := range found {
		note += " " + k
	}
	return doctorCheck{name: "provider api keys", ok: true, note: note}
}

func (c *DoctorCmd) checkSchemaVersion(path string) doctorCheck {
	store, err := eventlog.NewSQLite(path, eventlog.WithReadOnly())
	if err != nil {
		return doctorCheck{name: "schema version", note: "open log: " + err.Error()}
	}
	defer store.Close()
	v, err := eventlog.SchemaVersion(context.Background(), store)
	if err != nil {
		return doctorCheck{name: "schema version", note: err.Error()}
	}
	if uint32(v) != event.SchemaVersion {
		return doctorCheck{
			name: "schema version",
			note: fmt.Sprintf("on-disk=%d, binary=%d (run `starling migrate %s`)", v, event.SchemaVersion, path),
		}
	}
	return doctorCheck{name: "schema version", ok: true, note: fmt.Sprintf("v%d (matches binary)", v)}
}

func (c *DoctorCmd) checkValidate(path string) doctorCheck {
	store, err := eventlog.NewSQLite(path, eventlog.WithReadOnly())
	if err != nil {
		return doctorCheck{name: "chain validation", note: "open log: " + err.Error()}
	}
	defer store.Close()

	lister, ok := store.(eventlog.RunLister)
	if !ok {
		return doctorCheck{name: "chain validation", note: "log does not implement RunLister"}
	}
	runs, err := lister.ListRuns(context.Background())
	if err != nil {
		return doctorCheck{name: "chain validation", note: err.Error()}
	}
	if len(runs) == 0 {
		return doctorCheck{name: "chain validation", ok: true, note: "no runs to validate"}
	}
	for _, rs := range runs {
		evs, err := store.Read(context.Background(), rs.RunID)
		if err != nil {
			return doctorCheck{name: "chain validation", note: fmt.Sprintf("read %s: %v", rs.RunID, err)}
		}
		if err := eventlog.Validate(evs); err != nil {
			return doctorCheck{name: "chain validation", note: fmt.Sprintf("run %s: %v", rs.RunID, err)}
		}
	}
	return doctorCheck{name: "chain validation", ok: true, note: fmt.Sprintf("%d runs validated", len(runs))}
}
