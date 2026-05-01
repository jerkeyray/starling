package inspect

import (
	"context"
	"testing"

	"github.com/jerkeyray/starling/eventlog"
)

func TestBuildDiffRows_MatchingRunsClassifyAllRowsMatch(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	runID := "diff-match"
	seedRunStartedOnly(t, log, runID)
	appendTurnStarted(t, log, runID, 2)

	evs, err := log.Read(context.Background(), runID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	rows, summary := buildDiffRows(evs, evs)
	if summary.Matching != 2 || summary.Diverging != 0 || summary.OnlyA != 0 || summary.OnlyB != 0 {
		t.Fatalf("summary = %+v, want all matching", summary)
	}
	for _, r := range rows {
		if r.Class != "match" {
			t.Fatalf("seq=%d class=%q, want match", r.Seq, r.Class)
		}
	}
	if summary.FirstDivSeq != 0 {
		t.Fatalf("FirstDivSeq = %d, want 0", summary.FirstDivSeq)
	}
}

func TestBuildDiffRows_OneSidedRunsClassifyOnlyA(t *testing.T) {
	log := eventlog.NewInMemory()
	defer log.Close()
	seedRunStartedOnly(t, log, "a")
	seedRunStartedOnly(t, log, "b")
	appendTurnStarted(t, log, "b", 2)

	evsA, _ := log.Read(context.Background(), "a")
	evsB, _ := log.Read(context.Background(), "b")
	rows, summary := buildDiffRows(evsA, evsB)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if summary.OnlyB != 1 {
		t.Fatalf("OnlyB = %d, want 1", summary.OnlyB)
	}
	if summary.FirstDivSeq != 2 {
		t.Fatalf("FirstDivSeq = %d, want 2", summary.FirstDivSeq)
	}
}
