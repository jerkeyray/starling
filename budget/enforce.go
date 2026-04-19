package budget

import "time"

// Enforce checks usage against b and returns the first tripped limit,
// or ("", 0, 0) if nothing is tripped.
//
// Precedence: output_tokens → usd → wall_clock. Deterministic so two
// axes tripping in the same frame always pick the same winner — this
// matters for replay byte-identity.
//
// Pass a zero wallStart to skip the wall-clock check. An unknown model
// disables the USD check (cost lookup returns 0 for unknown models);
// callers get the same behaviour as if MaxUSD were zero for that call.
//
// Returned (cap, actual) are both reported as float64 to match the
// event.BudgetExceeded schema, which stores both in float64 because
// USD is a dollar amount and token/ms counts are small enough to fit
// without precision loss.
func Enforce(b Budget, model string, inTok, outTok int64, wallStart time.Time) (limit string, cap, actual float64) {
	if b.MaxOutputTokens > 0 && outTok > b.MaxOutputTokens {
		return "output_tokens", float64(b.MaxOutputTokens), float64(outTok)
	}
	if b.MaxUSD > 0 {
		if cost, known := CostUSD(model, inTok, outTok); known && cost > b.MaxUSD {
			return "usd", b.MaxUSD, cost
		}
	}
	if b.MaxWallClock > 0 && !wallStart.IsZero() {
		elapsed := time.Since(wallStart)
		if elapsed > b.MaxWallClock {
			return "wall_clock", float64(b.MaxWallClock.Milliseconds()), float64(elapsed.Milliseconds())
		}
	}
	return "", 0, 0
}
