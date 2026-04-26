package eventlog_test

import (
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

// FuzzValidate ensures Validate never panics on arbitrary input.
// The fuzzer drives single-event slices through; Validate's chain
// checks reject most inputs, but the corpus surfaces decode/payload
// edge cases reaching the semantic layer.
func FuzzValidate(f *testing.F) {
	seed, err := event.Marshal(event.Event{
		RunID:     "r1",
		Seq:       1,
		Timestamp: 1,
		Kind:      event.KindRunStarted,
	})
	if err != nil {
		f.Fatalf("seed: %v", err)
	}
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		ev, err := event.Unmarshal(data)
		if err != nil {
			return
		}
		_ = eventlog.Validate([]event.Event{ev}) // must not panic
	})
}
