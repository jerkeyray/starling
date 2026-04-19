package budget_test

import (
	"testing"
	"time"

	"github.com/jerkeyray/starling/budget"
)

// TestBudget_ZeroValueUnlimited pins the contract that step.LLMCall's
// pre-call check depends on: a zero-valued Budget imposes no caps on
// any axis. If someone flips these defaults a guard would have to
// appear in every caller; the zero value stays "unlimited".
func TestBudget_ZeroValueUnlimited(t *testing.T) {
	var b budget.Budget
	if b.MaxInputTokens != 0 {
		t.Errorf("MaxInputTokens = %d, want 0", b.MaxInputTokens)
	}
	if b.MaxOutputTokens != 0 {
		t.Errorf("MaxOutputTokens = %d, want 0", b.MaxOutputTokens)
	}
	if b.MaxUSD != 0 {
		t.Errorf("MaxUSD = %v, want 0", b.MaxUSD)
	}
	if b.MaxWallClock != 0 {
		t.Errorf("MaxWallClock = %v, want 0", b.MaxWallClock)
	}
}

// TestBudget_FieldsRoundTrip is a belt-and-braces check that the
// exported fields round-trip through struct assignment. It also locks
// in that MaxWallClock is a time.Duration rather than an int64 of ms
// — T11 enforcement code will depend on that.
func TestBudget_FieldsRoundTrip(t *testing.T) {
	b := budget.Budget{
		MaxInputTokens:  1_000,
		MaxOutputTokens: 2_000,
		MaxUSD:          0.50,
		MaxWallClock:    30 * time.Second,
	}
	if b.MaxInputTokens != 1_000 || b.MaxOutputTokens != 2_000 {
		t.Fatalf("token fields: %+v", b)
	}
	if b.MaxUSD != 0.50 {
		t.Fatalf("MaxUSD: %v", b.MaxUSD)
	}
	if b.MaxWallClock != 30*time.Second {
		t.Fatalf("MaxWallClock: %v", b.MaxWallClock)
	}
}
