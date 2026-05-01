package starlingtest

import (
	"context"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

// RunStartedOptions tunes the seeded RunStarted payload. Zero values
// fall back to "starlingtest"-flavored defaults so tests that don't
// care about the metadata can pass an empty struct.
type RunStartedOptions struct {
	Goal       string // default: "starlingtest run"
	ProviderID string // default: "scripted"
	APIVersion string // default: "v0"
	ModelID    string // default: "test-model"
}

// AppendRunStarted appends a single RunStarted event to log at Seq=1
// and returns it. Fails the test on encode/append errors.
//
// Use this when a test needs an event log seeded with a non-terminal
// run — e.g. to exercise live-tail subscribers, inspector views, or
// resume paths — without spinning up a full Agent.Run.
func AppendRunStarted(t *testing.T, log eventlog.EventLog, runID string, opts RunStartedOptions) event.Event {
	t.Helper()
	if opts.Goal == "" {
		opts.Goal = "starlingtest run"
	}
	if opts.ProviderID == "" {
		opts.ProviderID = "scripted"
	}
	if opts.APIVersion == "" {
		opts.APIVersion = "v0"
	}
	if opts.ModelID == "" {
		opts.ModelID = "test-model"
	}
	payload, err := event.EncodePayload(event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          opts.Goal,
		ProviderID:    opts.ProviderID,
		APIVersion:    opts.APIVersion,
		ModelID:       opts.ModelID,
	})
	if err != nil {
		t.Fatalf("starlingtest: encode RunStarted: %v", err)
	}
	ev := event.Event{
		RunID:     runID,
		Seq:       1,
		Timestamp: time.Now().UnixNano(),
		Kind:      event.KindRunStarted,
		Payload:   payload,
	}
	if err := log.Append(context.Background(), runID, ev); err != nil {
		t.Fatalf("starlingtest: append RunStarted: %v", err)
	}
	return ev
}
