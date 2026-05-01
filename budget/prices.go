package budget

import (
	"fmt"
	"io"
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
	"gpt-4o":        {InPerMtok: 2.50, OutPerMtok: 10.00},
	"gpt-4o-mini":   {InPerMtok: 0.15, OutPerMtok: 0.60},
	"gpt-4-turbo":   {InPerMtok: 10.00, OutPerMtok: 30.00},
	"gpt-3.5-turbo": {InPerMtok: 0.50, OutPerMtok: 1.50},

	// Anthropic Claude — USD per million tokens, sourced from
	// platform.claude.com/docs/en/about-claude/pricing. Prompt-caching
	// read/write multipliers are applied at the usage layer; these
	// entries reflect base input/output only.
	"claude-opus-4-7":   {InPerMtok: 5.00, OutPerMtok: 25.00},
	"claude-opus-4-6":   {InPerMtok: 5.00, OutPerMtok: 25.00},
	"claude-opus-4-5":   {InPerMtok: 5.00, OutPerMtok: 25.00},
	"claude-opus-4-1":   {InPerMtok: 15.00, OutPerMtok: 75.00},
	"claude-opus-4-0":   {InPerMtok: 15.00, OutPerMtok: 75.00},
	"claude-sonnet-4-6": {InPerMtok: 3.00, OutPerMtok: 15.00},
	"claude-sonnet-4-5": {InPerMtok: 3.00, OutPerMtok: 15.00},
	"claude-sonnet-4-0": {InPerMtok: 3.00, OutPerMtok: 15.00},
	"claude-haiku-4-5":  {InPerMtok: 1.00, OutPerMtok: 5.00},
	"claude-haiku-3-5":  {InPerMtok: 0.80, OutPerMtok: 4.00},
	"claude-haiku-3":    {InPerMtok: 0.25, OutPerMtok: 1.25},
}

var (
	pricesMu   sync.RWMutex
	warnOnce   sync.Map  // map[string]*sync.Once
	warnWriter io.Writer = os.Stderr
)

// RegisterPricing registers or overrides USD pricing for a model. Both
// inPerMtok and outPerMtok are quoted in dollars per million tokens to
// match vendor pricing pages.
//
// Calling RegisterPricing for a model that previously triggered the
// "no price entry" warning clears that one-shot memo so the next
// missing model still surfaces its own warning.
//
// Negative or zero rates are accepted; CostUSD will multiply through
// without complaint. The intended use is custom in-house models or
// new vendor models not yet shipped in the built-in table.
func RegisterPricing(model string, inPerMtok, outPerMtok float64) {
	pricesMu.Lock()
	prices[model] = price{InPerMtok: inPerMtok, OutPerMtok: outPerMtok}
	pricesMu.Unlock()
	warnOnce.Delete(model)
}

// CostUSD returns the dollar cost of a call of model consuming inTok
// input and outTok output tokens. The second return reports whether the
// model was known; unknown models return (0, false) and a one-shot
// warning is written to stderr.
func CostUSD(model string, inTok, outTok int64) (float64, bool) {
	pricesMu.RLock()
	p, ok := prices[model]
	pricesMu.RUnlock()
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
