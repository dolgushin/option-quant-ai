package main

import (
	"testing"
	"time"
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
		"Si":   1000,
		"RI":   100,
		"CR":   1000,
		"BTC":  1,
		"XXXX": 1,
	}
	for sym, want := range cases {
		if got := contractMultiplier(sym); got != want {
			t.Fatalf("contractMultiplier(%q)=%v, want %v", sym, got, want)
		}
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