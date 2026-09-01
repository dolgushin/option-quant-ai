package main

import (
	"math"
	"testing"

	"option-quant-ai/quant"
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

// P&L-now curve must pass through the position's actual market P&L at the
// current spot (not a recomputation of the mark from the local model), so the
// chart agrees with the totals/depth panel.
func TestAnalyticsPnlNowAnchoredToMarket(t *testing.T) {
	legs := analyticsTestLegs()
	// Simulate a live position where market marks moved away from entry:
	// short leg 6.32 -> 7.50, long leg 3.72 -> 3.80.
	legs[0].Current = 7.50
	legs[1].Current = 3.80
	mult := 100.0
	spot := 273.76
	a := buildSpreadAnalytics("SBERP", "2026-09-16", spot, 22, mult, legs)

	want := 0.0
	for _, l := range a.Legs {
		dir := 1.0
		if l.Side == "SELL" {
			dir = -1
		}
		want += dir * (l.Current - l.Entry) * float64(l.Quantity) * mult
	}
	want = math.Round(want*100) / 100
	if math.Abs(a.Totals["pnl"]-want) > 0.01 {
		t.Fatalf("totals pnl = %.2f, want market %.2f", a.Totals["pnl"], want)
	}
	// Find the grid index nearest the current spot and check the now curve.
	best := 0
	bestD := math.MaxFloat64
	pnlAtSpot := 0.0
	for i, S := range a.Curves.Spots {
		if d := math.Abs(S - spot); d < bestD {
			bestD = d
			best = i
			pnlAtSpot = a.Curves.PnlNow[i]
		}
	}
	if math.Abs(pnlAtSpot-a.Totals["pnl"]) > 20 {
		t.Fatalf("pnl_now at spot = %.2f (%d), totals = %.2f; curve not anchored", pnlAtSpot, best, a.Totals["pnl"])
	}
}

// Open-position legs enriched from the MOEX calculator must be re-marked at
// the exchange's theo so the "now" P&L matches the constructor (not a wide or
// stale live mid). Futures and unenriched legs are left untouched.
func TestAnalyticsOpenLegsPricedAtMOEXTheo(t *testing.T) {
	legs := analyticsTestLegs()
	legs[0].Moex = &moexLegData{Theo: 9.10}
	legs[1].Moex = &moexLegData{Theo: 4.40}
	legs = append(legs, analyticsLeg{SecID: "RIM1", Side: "BUY", Kind: "FUTURES", Strike: 0, Quantity: 1, Entry: 90000, Current: 90100})
	priceOpenLegsAtMOEXTheo(legs)
	if math.Abs(legs[0].Current-9.10) > 1e-9 {
		t.Fatalf("leg0 current = %.4f, want MOEX theo 9.10", legs[0].Current)
	}
	if math.Abs(legs[1].Current-4.40) > 1e-9 {
		t.Fatalf("leg1 current = %.4f, want MOEX theo 4.40", legs[1].Current)
	}
	if legs[2].Current != 90100 {
		t.Fatalf("futures leg current = %.4f, must stay untouched", legs[2].Current)
	}
}

func TestAnalyticsUsesMOEXGreeksAndIV(t *testing.T) {
	legs := analyticsTestLegs()
	// Simulate the MOEX Options Calculator enrichment: IV and greeks from the
	// exchange take precedence over the local Black-Scholes derivation.
	legs[0].Moex = &moexLegData{IV: 0.21, Theo: 6.5, Delta: 0.35, Gamma: 0.02, Vega: 0.55, Theta: 0.9, Rho: 0.1}
	legs[1].Moex = &moexLegData{IV: 0.19, Theo: 3.9, Delta: 0.25, Gamma: 0.02, Vega: 0.52, Theta: 0.7, Rho: 0.08}
	a := buildSpreadAnalytics("SBERP", "2026-09-16", 273.76, 22, 100, legs)
	if math.Abs(a.Legs[0].Iv-21) > 0.01 {
		t.Fatalf("leg0 iv = %.2f, want 21 (from MOEX)", a.Legs[0].Iv)
	}
	// Delta signs follow side: sell -> negative, buy -> positive.
	if a.Legs[0].Delta >= 0 {
		t.Fatalf("leg0 delta = %.4f, want negative (sell)", a.Legs[0].Delta)
	}
	if a.Legs[1].Delta <= 0 {
		t.Fatalf("leg1 delta = %.4f, want positive (buy)", a.Legs[1].Delta)
	}
	// Buy call at slightly-LT strike: MOEX greeks used (0.25 * 2).
	if math.Abs(a.Legs[1].Delta-0.50) > 1e-6 {
		t.Fatalf("leg1 delta = %.4f, want 0.50", a.Legs[1].Delta)
	}
}

// theoMarkedOpenPosition without a MOEX calculator falls back to the passed
// hybrid marks — the position is priced exactly the way it was re-priced.
func TestTheoMarkedOpenPositionFallback(t *testing.T) {
	cur, pnl, bySecid, applied := theoMarkedOpenPosition("Si", "2026-12-17",
		[]quant.PositionLeg{
			{SecID: "Si86500BX6", Side: "SELL", Kind: "OPTION", Strike: 86500, IsCall: false, Quantity: 4, EntryPrice: 2534, CurrentPrice: 2500},
			{SecID: "Si86000BX6", Side: "BUY", Kind: "OPTION", Strike: 86000, IsCall: false, Quantity: 4, EntryPrice: 2000, CurrentPrice: 2350},
		}, 1000, -2136, 1)
	if applied {
		t.Fatalf("applied = true without a MOEX calculator, want fallback")
	}
	if math.Abs(cur-1000) > 1e-9 || math.Abs(pnl-3136) > 1e-9 {
		t.Fatalf("fallback cur/pnl = %v / %v, want hybrid 1000 / 3136", cur, pnl)
	}
	if len(bySecid) != 0 {
		t.Fatalf("bySecid must be empty on fallback, got %v", bySecid)
	}
}
