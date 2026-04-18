package budget

import (
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

func TestCostUSD_Unknown(t *testing.T) {
	got, ok := CostUSD("mystery-model-x", 1000, 1000)
	if ok {
		t.Fatalf("ok = true, want false for unknown model")
	}
	if got != 0 {
		t.Fatalf("CostUSD = %v, want 0", got)
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
