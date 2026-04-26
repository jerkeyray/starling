package event_test

import (
	"testing"

	"github.com/jerkeyray/starling/event"
)

// FuzzUnmarshal exercises Unmarshal with arbitrary bytes; it must not
// panic, and must either return an error or a struct that round-trips
// through Marshal.
func FuzzUnmarshal(f *testing.F) {
	seed, err := event.Marshal(event.Event{
		RunID:     "r1",
		Seq:       1,
		Timestamp: 1,
		Kind:      event.KindRunStarted,
	})
	if err != nil {
		f.Fatalf("seed Marshal: %v", err)
	}
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		ev, err := event.Unmarshal(data)
		if err != nil {
			return
		}
		// Round-trip: a successful decode must re-encode without error.
		if _, err := event.Marshal(ev); err != nil {
			t.Fatalf("round-trip Marshal failed: %v", err)
		}
	})
}

// FuzzToJSON exercises ToJSON across the full Event surface.
// Successful Unmarshal must yield a struct ToJSON can render or reject
// cleanly.
func FuzzToJSON(f *testing.F) {
	seed, err := event.Marshal(event.Event{
		RunID:     "r1",
		Seq:       1,
		Timestamp: 1,
		Kind:      event.KindRunStarted,
	})
	if err != nil {
		f.Fatalf("seed Marshal: %v", err)
	}
	f.Add(seed)

	f.Fuzz(func(t *testing.T, data []byte) {
		ev, err := event.Unmarshal(data)
		if err != nil {
			return
		}
		_, _ = event.ToJSON(ev) // must not panic
	})
}
