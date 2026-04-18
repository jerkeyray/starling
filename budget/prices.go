package budget

import (
	"fmt"
	"os"
	"sync"
)

// price is a per-model USD price in dollars per million tokens.
type price struct {
	InPerMtok  float64
	OutPerMtok float64
}

// prices is a minimal per-model USD price table. Keep values in dollars
// per million tokens to match vendor pricing pages; the lookup below
// converts to a per-token multiplier.
//
// Extend as needed. Unknown models fall through to (0, false) — see
// CostUSD — so a missing entry never blocks a run, it just reports $0
// and logs one warning per model.
var prices = map[string]price{
	"gpt-4o":       {InPerMtok: 2.50, OutPerMtok: 10.00},
	"gpt-4o-mini":  {InPerMtok: 0.15, OutPerMtok: 0.60},
	"gpt-4-turbo":  {InPerMtok: 10.00, OutPerMtok: 30.00},
	"gpt-3.5-turbo": {InPerMtok: 0.50, OutPerMtok: 1.50},
}

var (
	warnOnce   sync.Map // map[string]*sync.Once
	warnWriter = os.Stderr
)

// CostUSD returns the dollar cost of a call of model consuming inTok
// input and outTok output tokens. The second return reports whether the
// model was known; unknown models return (0, false) and a one-shot
// warning is written to stderr.
func CostUSD(model string, inTok, outTok int64) (float64, bool) {
	p, ok := prices[model]
	if !ok {
		once, _ := warnOnce.LoadOrStore(model, &sync.Once{})
		once.(*sync.Once).Do(func() {
			fmt.Fprintf(warnWriter, "budget: no price entry for model %q; cost_usd will be reported as 0\n", model)
		})
		return 0, false
	}
	cost := (float64(inTok)*p.InPerMtok + float64(outTok)*p.OutPerMtok) / 1_000_000.0
	return cost, true
}
