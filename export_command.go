package starling

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jerkeyray/starling/event"
)

// ExportCommand returns a CLI-style entrypoint for `starling export`.
// Emits one NDJSON line per event (envelope + typed payload) so the
// output pipes cleanly into jq. Intended to be invoked from
// cmd/starling; the returned *ExportCmd is safe to configure further
// before Run.
func ExportCommand() *ExportCmd {
	return &ExportCmd{Name: "export", Output: os.Stdout}
}

// ExportCmd is the handle returned by ExportCommand.
type ExportCmd struct {
	// Name is used in flag error messages and the usage string.
	Name string
	// Output is where NDJSON is written. Defaults to os.Stdout.
	Output io.Writer
}

// exportLine is the wire shape of one NDJSON line from `starling
// export`. `payload` is the typed per-kind payload in JSON form —
// nested directly so `jq '.payload.text'` works without an inner
// decode step. Envelope fields use the same casing as the inspector
// UI / event.ToJSON (snake_case).
type exportLine struct {
	RunID    string          `json:"run_id"`
	Seq      uint64          `json:"seq"`
	Ts       int64           `json:"ts"`
	Kind     string          `json:"kind"`
	PrevHash []byte          `json:"prev_hash"`
	Payload  json.RawMessage `json:"payload"`
}

// Run parses args and writes one NDJSON line per event in the run.
//
// args shape: <db> <runID>
func (c *ExportCmd) Run(args []string) error {
	if c.Name == "" {
		c.Name = "export"
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(c.Output)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s <db> <runID>\n\n", c.Name)
		fmt.Fprintln(fs.Output(), "Dump every event of <runID> as NDJSON (one line per event).")
	}
	if err := fs.Parse(args); err != nil {
		return err
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
	evs, err := store.Read(ctx, fs.Arg(1))
	if err != nil {
		return fmt.Errorf("read log: %w", err)
	}
	if len(evs) == 0 {
		return fmt.Errorf("run %q not found", fs.Arg(1))
	}

	enc := json.NewEncoder(c.Output)
	// Do not escape HTML — event payloads sometimes carry URLs and
	// users piping into jq expect verbatim output.
	enc.SetEscapeHTML(false)
	for _, ev := range evs {
		payload, err := event.ToJSON(ev)
		if err != nil {
			return fmt.Errorf("encode payload (seq=%d kind=%s): %w", ev.Seq, ev.Kind, err)
		}
		line := exportLine{
			RunID:    ev.RunID,
			Seq:      ev.Seq,
			Ts:       ev.Timestamp,
			Kind:     ev.Kind.String(),
			PrevHash: ev.PrevHash,
			Payload:  payload,
		}
		if err := enc.Encode(&line); err != nil {
			return fmt.Errorf("write line (seq=%d): %w", ev.Seq, err)
		}
	}
	return nil
}
