package eventlog

import "github.com/jerkeyray/starling/event"

// aggregateRun computes per-run totals over a hash-chained event slice,
// shared by every backend's ListRuns. evs must be in sequence order
// and belong to the same run.
//
// Returned values are best-effort: an event whose payload fails to
// decode (corrupt entry, unknown payload version on disk) is skipped
// rather than failing the whole aggregation, since ListRuns is a UI
// helper and a single broken row should not blank the dashboard.
func aggregateRun(evs []event.Event) (turns, tools int, inputTokens, outputTokens int64, costUSD float64, durationNs int64) {
	if len(evs) == 0 {
		return
	}
	first := evs[0]
	last := evs[len(evs)-1]
	if last.Timestamp >= first.Timestamp {
		durationNs = last.Timestamp - first.Timestamp
	}
	for i := range evs {
		switch evs[i].Kind {
		case event.KindTurnStarted:
			turns++
		case event.KindToolCallScheduled:
			tools++
		case event.KindAssistantMessageCompleted:
			amc, err := evs[i].AsAssistantMessageCompleted()
			if err != nil {
				continue
			}
			inputTokens += amc.InputTokens
			outputTokens += amc.OutputTokens
			costUSD += amc.CostUSD
		}
	}
	return
}
