//go:build soak

package eventlog_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jerkeyray/starling/eventlog"
)

const soakRunEvents = 10_000

// TestSoak_LargeRun_SQLite appends a 10k-event run end-to-end against
// SQLite, then reads it back and validates the chain. Catches O(n)
// regressions in the append path and the Read materialization.
func TestSoak_LargeRun_SQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "soak.db")
	log, err := eventlog.NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	cb := &chainBuilder{}
	ctx := context.Background()
	for i := 0; i < soakRunEvents; i++ {
		if err := log.Append(ctx, "soak-1", cb.next(t, "soak-1", "msg")); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	got, err := log.Read(ctx, "soak-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != soakRunEvents {
		t.Fatalf("len(got) = %d, want %d", len(got), soakRunEvents)
	}
}

// TestSoak_MemoryFootprint_InMemory builds a 10k-event in-memory run
// and reports allocator stats. Not an assertion — the watch is for
// surprising deltas vs the previous build.
func TestSoak_MemoryFootprint_InMemory(t *testing.T) {
	log := eventlog.NewInMemory()
	t.Cleanup(func() { _ = log.Close() })

	cb := &chainBuilder{}
	ctx := context.Background()
	for i := 0; i < soakRunEvents; i++ {
		if err := log.Append(ctx, "soak-mem", cb.next(t, "soak-mem", "msg")); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("after %d events: HeapAlloc=%d HeapInuse=%d", soakRunEvents, m.HeapAlloc, m.HeapInuse)
}
