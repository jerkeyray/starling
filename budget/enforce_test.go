package budget

import (
	"testing"
	"time"
)

func TestEnforce_NoTrip(t *testing.T) {
	b := Budget{MaxOutputTokens: 1000, MaxUSD: 10.00, MaxWallClock: time.Second}
	limit, _, _ := Enforce(b, "gpt-4o-mini", 100, 50, time.Now())
	if limit != "" {
		t.Fatalf("limit = %q, want empty", limit)
	}
}

func TestEnforce_OutputTokens(t *testing.T) {
	b := Budget{MaxOutputTokens: 5}
	limit, cap, actual := Enforce(b, "gpt-4o-mini", 0, 20, time.Time{})
	if limit != "output_tokens" {
		t.Fatalf("limit = %q", limit)
	}
	if cap != 5 || actual != 20 {
		t.Fatalf("cap=%v actual=%v", cap, actual)
	}
}

func TestEnforce_USD(t *testing.T) {
	// gpt-4o-mini = $0.15 in / $0.60 out per M tokens.
	// 1M out = $0.60, cap $0.0001 → trips.
	b := Budget{MaxUSD: 0.0001}
	limit, cap, actual := Enforce(b, "gpt-4o-mini", 0, 1_000_000, time.Time{})
	if limit != "usd" {
		t.Fatalf("limit = %q", limit)
	}
	if cap != 0.0001 || actual <= cap {
		t.Fatalf("cap=%v actual=%v", cap, actual)
	}
}

func TestEnforce_USDUnknownModelSkips(t *testing.T) {
	b := Budget{MaxUSD: 0.0001}
	limit, _, _ := Enforce(b, "mystery-model", 1_000_000, 1_000_000, time.Time{})
	if limit != "" {
		t.Fatalf("limit = %q, want empty (unknown model → skip)", limit)
	}
}

func TestEnforce_WallClock(t *testing.T) {
	b := Budget{MaxWallClock: 10 * time.Millisecond}
	start := time.Now().Add(-50 * time.Millisecond) // simulate 50ms elapsed
	limit, cap, actual := Enforce(b, "gpt-4o-mini", 0, 0, start)
	if limit != "wall_clock" {
		t.Fatalf("limit = %q", limit)
	}
	if cap != 10 || actual < 50 {
		t.Fatalf("cap=%v actual=%v, want cap=10 actual>=50", cap, actual)
	}
}

func TestEnforce_WallClockZeroStartSkips(t *testing.T) {
	b := Budget{MaxWallClock: time.Nanosecond}
	limit, _, _ := Enforce(b, "gpt-4o-mini", 0, 0, time.Time{})
	if limit != "" {
		t.Fatalf("limit = %q, want empty (zero wallStart → skip)", limit)
	}
}

func TestEnforce_Precedence_OutputTokensBeatsUSD(t *testing.T) {
	// Both trip. output_tokens must win.
	b := Budget{MaxOutputTokens: 1, MaxUSD: 0.0001}
	limit, _, _ := Enforce(b, "gpt-4o-mini", 1_000_000, 1_000_000, time.Time{})
	if limit != "output_tokens" {
		t.Fatalf("limit = %q, want output_tokens (beats usd)", limit)
	}
}

func TestEnforce_Precedence_USDBeatsWallClock(t *testing.T) {
	b := Budget{MaxUSD: 0.0001, MaxWallClock: time.Nanosecond}
	start := time.Now().Add(-time.Second)
	limit, _, _ := Enforce(b, "gpt-4o-mini", 0, 1_000_000, start)
	if limit != "usd" {
		t.Fatalf("limit = %q, want usd (beats wall_clock)", limit)
	}
}
