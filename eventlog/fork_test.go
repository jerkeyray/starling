package eventlog_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
)

func TestForkSQLite_TruncatesAtBeforeSeq(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	dst := filepath.Join(dir, "dst.db")

	srcLog, err := eventlog.NewSQLite(src)
	if err != nil {
		t.Fatalf("NewSQLite src: %v", err)
	}
	const runID = "fork-run-1"
	seedRun(t, srcLog, runID, 5)
	if err := srcLog.Close(); err != nil {
		t.Fatalf("close src: %v", err)
	}

	if err := eventlog.ForkSQLite(context.Background(), src, dst, runID, 3); err != nil {
		t.Fatalf("ForkSQLite: %v", err)
	}

	dstLog, err := eventlog.NewSQLite(dst)
	if err != nil {
		t.Fatalf("NewSQLite dst: %v", err)
	}
	t.Cleanup(func() { _ = dstLog.Close() })

	evs, err := dstLog.Read(context.Background(), runID)
	if err != nil {
		t.Fatalf("Read dst: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("kept %d events, want 2 (seq < 3)", len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("evs[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestForkSQLite_PreservesWALCorrectly(t *testing.T) {
	// Reproduce the WAL footgun: keep the source open (and thus
	// producing -wal/-shm sidecars) while we fork. A naïve cp would
	// miss in-WAL writes; VACUUM INTO must include them.
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	dst := filepath.Join(dir, "dst.db")

	srcLog, err := eventlog.NewSQLite(src)
	if err != nil {
		t.Fatalf("NewSQLite src: %v", err)
	}
	t.Cleanup(func() { _ = srcLog.Close() })

	const runID = "fork-run-wal"
	seedRun(t, srcLog, runID, 4)

	if err := eventlog.ForkSQLite(context.Background(), src, dst, runID, 0); err != nil {
		t.Fatalf("ForkSQLite: %v", err)
	}
	for _, sidecar := range []string{dst + "-wal", dst + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Fatalf("destination %s leaked", sidecar)
		}
	}

	dstLog, err := eventlog.NewSQLite(dst)
	if err != nil {
		t.Fatalf("NewSQLite dst: %v", err)
	}
	t.Cleanup(func() { _ = dstLog.Close() })
	evs, err := dstLog.Read(context.Background(), runID)
	if err != nil {
		t.Fatalf("Read dst: %v", err)
	}
	if len(evs) != 4 {
		t.Fatalf("kept %d events, want 4 (beforeSeq=0 keeps all)", len(evs))
	}
}

func TestForkSQLite_RefusesExistingDest(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	dst := filepath.Join(dir, "dst.db")

	srcLog, err := eventlog.NewSQLite(src)
	if err != nil {
		t.Fatalf("NewSQLite src: %v", err)
	}
	seedRun(t, srcLog, "r", 1)
	_ = srcLog.Close()

	if err := os.WriteFile(dst, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	if err := eventlog.ForkSQLite(context.Background(), src, dst, "r", 0); err == nil {
		t.Fatal("expected error on pre-existing dst")
	}
}

func TestForkSQLite_UnknownRunReturnsErrForkNotFound(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	dst := filepath.Join(dir, "dst.db")

	srcLog, err := eventlog.NewSQLite(src)
	if err != nil {
		t.Fatalf("NewSQLite src: %v", err)
	}
	seedRun(t, srcLog, "actual-run", 1)
	_ = srcLog.Close()

	err = eventlog.ForkSQLite(context.Background(), src, dst, "missing", 0)
	if !errors.Is(err, eventlog.ErrForkNotFound) {
		t.Fatalf("err = %v, want ErrForkNotFound", err)
	}
}

// seedRun appends n hash-chained RunStarted+Custom events (kind doesn't
// matter for fork tests). Returns when n events have been written.
func seedRun(t *testing.T, log eventlog.EventLog, runID string, n int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixNano()

	rsPayload, err := event.EncodePayload(event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          "fork test",
		ProviderID:    "scripted",
		APIVersion:    "v0",
		ModelID:       "m",
	})
	if err != nil {
		t.Fatalf("encode RunStarted: %v", err)
	}
	first := event.Event{
		RunID:     runID,
		Seq:       1,
		Timestamp: now,
		Kind:      event.KindRunStarted,
		Payload:   rsPayload,
	}
	if err := log.Append(ctx, runID, first); err != nil {
		t.Fatalf("append seq=1: %v", err)
	}
	prev := first
	for i := 2; i <= n; i++ {
		tsPayload, err := event.EncodePayload(event.TurnStarted{TurnID: "t"})
		if err != nil {
			t.Fatalf("encode TurnStarted: %v", err)
		}
		prevEnc, _ := event.Marshal(prev)
		ev := event.Event{
			RunID:     runID,
			Seq:       uint64(i),
			PrevHash:  event.Hash(prevEnc),
			Timestamp: now + int64(i),
			Kind:      event.KindTurnStarted,
			Payload:   tsPayload,
		}
		if err := log.Append(ctx, runID, ev); err != nil {
			t.Fatalf("append seq=%d: %v", i, err)
		}
		prev = ev
	}
}
