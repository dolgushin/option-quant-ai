package main

import (
	"math"
	"testing"

	"option-quant-ai/quant"
)

// theoTestLegs returns legs whose CurrentPrice exactly matches a Black-Scholes
// price at a chosen IV, so spreadTheoLive must reproduce the market quote: the
// fair value (per multiplier) equals the sum of leg quote values and any
// discrepancy stays within bisection rounding.
func theoTestLegs() []quant.PositionLeg {
	return []quant.PositionLeg{
		{SecID: "SP280CI6", Symbol: "SBERP", Kind: "OPTION", Side: "SELL", Quantity: 1, Strike: 280, IsCall: true, EntryPrice: 6.32, CurrentPrice: 6.32},
		{SecID: "SP290CI6", Symbol: "SBERP", Kind: "OPTION", Side: "BUY", Quantity: 1, Strike: 290, IsCall: true, EntryPrice: 3.72, CurrentPrice: 3.72},
	}
}

func TestSpreadTheoLiveTracksMarket(t *testing.T) {
	// SBERP: spot 273.76, 22 DTE, multiplier 100, single lot.
	th := spreadTheoLive(theoTestLegs(), 273.76, "2026-09-16", 100.0)

	// Market value of the spread at quotes (signed: SELL @ 6.32, BUY @ 3.72),
	// lot 1, multiplier 100.
	wallet := (-6.32 + 3.72) * 100.0
	if got := math.Round(th.Value); math.Abs(got-wallet) > 5 {
		t.Fatalf("theo value = %.2f, want ≈ %.2f (must mirror market quote)", th.Value, wallet)
	}

	// Vega direction for a bear call (short near ATM, long far OTM): net short
	// vega -> value falls as IV rises -> Vega total must be negative.
	if th.Vega > 0 {
		t.Fatalf("bear call must have net negative vega sensitivity, got %+.2f", th.Vega)
	}
}

func TestSpreadTheoLiveScalesByQuantity(t *testing.T) {
	legs := theoTestLegs()
	legs[0].Quantity = 2
	legs[1].Quantity = 2
	one := spreadTheoLive(theoTestLegs(), 273.76, "2026-09-16", 100.0)
	two := spreadTheoLive(legs, 273.76, "2026-09-16", 100.0)
	if math.Abs(two.Value-2*one.Value) > 10 {
		t.Fatalf("doubling quantity should double value: one=%.2f two=%.2f", one.Value, two.Value)
	}
	if math.Abs(two.Vega-2*one.Vega) > 1 {
		t.Fatalf("doubling quantity should double vega: one=%.2f two=%.2f", one.Vega, two.Vega)
	}
}
