package main

import (
	"math"
	"testing"
)

func analyticsTestLegs() []analyticsLeg {
	// Bear call spread on SBERP: SELL 280C @ 6.32, BUY 290C @ 3.72, 2 lots.
	return []analyticsLeg{
		{SecID: "SP280CI6", Side: "SELL", Kind: "OPTION", Strike: 280, IsCall: true, Quantity: 2, Entry: 6.32, Current: 6.32},
		{SecID: "SP290CI6", Side: "BUY", Kind: "OPTION", Strike: 290, IsCall: true, Quantity: 2, Entry: 3.72, Current: 3.72},
	}
}

func TestAnalyticsCurvesShape(t *testing.T) {
	a := buildSpreadAnalytics("SBERP", "2026-09-16", 273.76, 22, 100, analyticsTestLegs())
	if len(a.Curves.Spots) != 61 {
		t.Fatalf("spots = %d, want 61", len(a.Curves.Spots))
	}
	if a.Legs[0].Iv <= 0 || a.Legs[1].Iv <= 0 {
		t.Fatalf("leg IVs not derived: %v %v", a.Legs[0].Iv, a.Legs[1].Iv)
	}
	// P&L now must cross zero at the entry spot (theo == current at the
	// derived IV). Grid step ~1₽ × money delta ~16 ₽/point allows a residual.
	minAbs := math.MaxFloat64
	for i := range a.Curves.Spots {
		minAbs = math.Min(minAbs, math.Abs(a.Curves.PnlNow[i]))
	}
	if minAbs > 12 {
		t.Fatalf("min |pnl_now| = %.2f, want ~0 near entry spot", minAbs)
	}
}

func TestAnalyticsExpiryPayoff(t *testing.T) {
	a := buildSpreadAnalytics("SBERP", "2026-09-16", 273.76, 22, 100, analyticsTestLegs())
	mult := 100.0
	q := 2.0
	credit := (6.32 - 3.72) * q * mult // 520
	width := (290.0 - 280.0) * q * mult
	// Deep below both strikes: full credit kept.
	low := a.Curves.PnlExpiry[0]
	if math.Abs(low-credit) > 1 {
		t.Fatalf("pnl_expiry low = %.2f, want ~%.2f", low, credit)
	}
	// Deep above both strikes: credit - width.
	high := a.Curves.PnlExpiry[len(a.Curves.PnlExpiry)-1]
	if math.Abs(high-(credit-width)) > 1 {
		t.Fatalf("pnl_expiry high = %.2f, want ~%.2f", high, credit-width)
	}
}

func TestAnalyticsTotalsPnLZeroAtEntry(t *testing.T) {
	a := buildSpreadAnalytics("SBERP", "2026-09-16", 273.76, 22, 100, analyticsTestLegs())
	if math.Abs(a.Totals["pnl"]) > 0.01 {
		t.Fatalf("totals pnl = %.4f at entry marks, want 0", a.Totals["pnl"])
	}
	// Short call delta negative, position delta between -2 and 0.
	if a.Totals["delta"] >= 0 || a.Totals["delta"] < -2 {
		t.Fatalf("totals delta = %.4f out of range", a.Totals["delta"])
	}
}
