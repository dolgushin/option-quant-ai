package main

import (
	"testing"
	"time"

	"option-quant-ai/quant"
)

func TestDTEInDays(t *testing.T) {
	ref := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	// 2026-09-17 is exactly 30 calendar days after 2026-08-18; the int()
	// truncation in dteInDays may yield 29 or 30 due to float precision.
	if got := dteInDays("2026-09-17", ref); got != 29 && got != 30 {
		t.Fatalf("dte for ~30 days out should be 29..30, got %d", got)
	}
	if got := dteInDays("2026-08-18", ref); got != 0 {
		t.Fatalf("dte=0 for same day, got %d", got)
	}
	if got := dteInDays("not-a-date", ref); got != 0 {
		t.Fatalf("invalid date should give 0, got %d", got)
	}
}

func TestContractMultiplier(t *testing.T) {
	cases := map[string]float64{
		"Si":   1,    // 1 premium point = 1 ₽ (ISS: MINSTEP 1, STEPPRICE 1)
		"CR":   1000, // MINSTEP 0.001, STEPPRICE 1
		"BTC":  1,
		"XXXX": 1,
	}
	for sym, want := range cases {
		if got := contractMultiplier(sym); got != want {
			t.Fatalf("contractMultiplier(%q)=%v, want %v", sym, got, want)
		}
	}
	// RI floats with USD/RUB: live value ≈ 1.66 ₽/point.
	if ri := contractMultiplier("RI"); ri <= 0 || ri > 10 {
		t.Fatalf("contractMultiplier(RI)=%v out of sane range", ri)
	}
}

func TestNearestStrike(t *testing.T) {
	chain := []optionContract{
		{Strike: 82000},
		{Strike: 83000},
		{Strike: 84000},
		{Strike: 85000},
	}

	if got := nearestStrike(chain, 83200); got != 83000 {
		t.Fatalf("nearest to 83200 should be 83000, got %v", got)
	}
	if got := nearestStrike(chain, 84900); got != 85000 {
		t.Fatalf("nearest to 84900 should be 85000, got %v", got)
	}
	if got := nearestStrike(nil, 83200); got != 83200 {
		t.Fatalf("empty chain should return spot, got %v", got)
	}
}

// TestProfileRangeAnchorsOnStrikes pins the payoff-chart window: a narrow
// vertical wing must fill the chart instead of drowning in ±20% of spot
// (the old behaviour rendered the kink as a single pixel — a flat line).
func TestProfileRangeAnchorsOnStrikes(t *testing.T) {
	legs := []quant.PositionLeg{
		{Kind: "OPTION", Side: "SELL", Strike: 86500},
		{Kind: "OPTION", Side: "BUY", Strike: 86000},
	}
	lo, hi := profileRange(legs, 86200)
	// Wing 500 → pad = max(300, 3460) = 3460; window ≈ [82540, 89960].
	if lo > 86000 || hi < 86500 {
		t.Fatalf("window [%v, %v] must contain both strikes", lo, hi)
	}
	if width := hi - lo; width > 10000 {
		t.Fatalf("window width %v far too wide for a 500 wing (was ±20%% of spot)", width)
	}
	// Futures-only legs anchor on entry.
	flegs := []quant.PositionLeg{{Kind: "FUTURES", Side: "BUY", EntryPrice: 86000}}
	lo, hi = profileRange(flegs, 90000)
	if lo > 86000 || hi < 86000 {
		t.Fatalf("futures window [%v, %v] must contain entry 86000", lo, hi)
	}
	// Nothing to anchor on → ±20% of spot fallback.
	lo, hi = profileRange(nil, 100)
	if lo != 80 || hi != 120 {
		t.Fatalf("fallback window = [%v, %v], want [80, 120]", lo, hi)
	}
}
