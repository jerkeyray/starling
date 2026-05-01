package budget

import (
	"bytes"
	"io"
	"math"
	"testing"
)

func TestCostUSD_Known(t *testing.T) {
	// gpt-4o-mini: $0.15 per 1M input, $0.60 per 1M output
	// 1M in + 1M out => $0.75
	got, ok := CostUSD("gpt-4o-mini", 1_000_000, 1_000_000)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	want := 0.75
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("CostUSD = %v, want %v", got, want)
	}
}

func TestCostUSD_Claude(t *testing.T) {
	cases := []struct {
		model           string
		wantIn, wantOut float64
	}{
		{"claude-opus-4-7", 5.00, 25.00},
		{"claude-sonnet-4-6", 3.00, 15.00},
		{"claude-haiku-4-5", 1.00, 5.00},
		{"claude-haiku-3", 0.25, 1.25},
	}
	for _, tc := range cases {
		// 1M in + 1M out => wantIn + wantOut dollars
		got, ok := CostUSD(tc.model, 1_000_000, 1_000_000)
		if !ok {
			t.Fatalf("%s: ok=false, want true", tc.model)
		}
		want := tc.wantIn + tc.wantOut
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s: CostUSD = %v, want %v", tc.model, got, want)
		}
	}
}

func TestCostUSD_Unknown(t *testing.T) {
	got, ok := CostUSD("mystery-model-x", 1000, 1000)
	if ok {
		t.Fatalf("ok = true, want false for unknown model")
	}
	if got != 0 {
		t.Fatalf("CostUSD = %v, want 0", got)
	}
}

func TestRegisterPricing_LooksUp(t *testing.T) {
	const model = "custom/test-model-A"
	RegisterPricing(model, 4.0, 12.0)
	t.Cleanup(func() {
		pricesMu.Lock()
		delete(prices, model)
		pricesMu.Unlock()
	})

	got, ok := CostUSD(model, 1_000_000, 1_000_000)
	if !ok {
		t.Fatalf("ok = false after RegisterPricing")
	}
	if math.Abs(got-16.0) > 1e-9 {
		t.Fatalf("CostUSD = %v, want 16.0", got)
	}
}

func TestRegisterPricing_OverridesAndClearsWarnOnce(t *testing.T) {
	const model = "custom/test-model-B"

	var buf bytes.Buffer
	prevWriter := warnWriter
	warnWriter = io.Writer(&buf)
	t.Cleanup(func() {
		warnWriter = prevWriter
		pricesMu.Lock()
		delete(prices, model)
		pricesMu.Unlock()
		warnOnce.Delete(model)
	})

	if _, ok := CostUSD(model, 1, 1); ok {
		t.Fatal("first lookup: ok=true, want false")
	}
	if buf.Len() == 0 {
		t.Fatal("expected one stderr warning on first miss")
	}

	RegisterPricing(model, 1.0, 1.0)

	if _, ok := CostUSD(model, 1, 1); !ok {
		t.Fatal("after RegisterPricing: ok=false")
	}

	// Drop the override; the next miss must re-arm the one-shot warning.
	pricesMu.Lock()
	delete(prices, model)
	pricesMu.Unlock()
	warnOnce.Delete(model)

	buf.Reset()
	if _, ok := CostUSD(model, 1, 1); ok {
		t.Fatal("post-delete: ok=true")
	}
	if buf.Len() == 0 {
		t.Fatal("expected re-armed warning after RegisterPricing cleared the memo")
	}
}

func TestCostUSD_Zero(t *testing.T) {
	got, ok := CostUSD("gpt-4o", 0, 0)
	if !ok {
		t.Fatalf("ok = false for known model")
	}
	if got != 0 {
		t.Fatalf("CostUSD = %v, want 0", got)
	}
}
