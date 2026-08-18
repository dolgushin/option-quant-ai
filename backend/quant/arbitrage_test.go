package quant

import (
	"math"
	"testing"
)

// TestCheckPutCallParityNoArbitrage verifies a fair-priced chain produces no signal.
func TestCheckPutCallParityNoArbitrage(t *testing.T) {
	spot, strike, days, rf := 83200.0, 83000.0, 30.0, 0.16
	tm := days / 365.0

	// Fair prices: C - P = S - K*e^(-rT)
	theoDiff := spot - strike*math.Exp(-rf*tm)
	callPrice := theoDiff + 100.0 // synthetic put+diff
	putPrice := 100.0

	o := CheckPutCallParity("Si", spot, strike, days, callPrice, putPrice, rf)
	if o.Strategy != "No Arbitrage" {
		t.Fatalf("expected No Arbitrage, got %s (spread=%.2f)", o.Strategy, o.Spread)
	}
	if o.ExpectedProfit != 0 {
		t.Fatalf("expected zero profit, got %.2f", o.ExpectedProfit)
	}
}

// TestCheckPutCallParityConversion verifies overpriced call triggers Conversion.
func TestCheckPutCallParityConversion(t *testing.T) {
	spot, strike, days, rf := 83200.0, 83000.0, 30.0, 0.16
	tm := days / 365.0
	theoDiff := spot - strike*math.Exp(-rf*tm)

	// Call is overpriced by 50.
	callPrice := theoDiff + 100.0 + 50.0
	putPrice := 100.0

	o := CheckPutCallParity("Si", spot, strike, days, callPrice, putPrice, rf)
	if o.Strategy == "No Arbitrage" {
		t.Fatal("expected Conversion signal for overpriced call")
	}
	if o.ExpectedProfit < 50 {
		t.Fatalf("expected profit ~50, got %.2f", o.ExpectedProfit)
	}
}

// TestCheckPutCallParityReversal verifies overpriced put triggers Reversal.
func TestCheckPutCallParityReversal(t *testing.T) {
	spot, strike, days, rf := 83200.0, 83000.0, 30.0, 0.16
	tm := days / 365.0
	theoDiff := spot - strike*math.Exp(-rf*tm)

	callPrice := theoDiff + 100.0
	putPrice := 100.0 + 50.0 // put overpriced

	o := CheckPutCallParity("Si", spot, strike, days, callPrice, putPrice, rf)
	if o.Strategy == "No Arbitrage" {
		t.Fatal("expected Reversal signal for overpriced put")
	}
	if o.ExpectedProfit < 50 {
		t.Fatalf("expected profit ~50, got %.2f", o.ExpectedProfit)
	}
}

// TestGetMOEXSpec verifies Si vs RI contract parameters.
func TestGetMOEXSpec(t *testing.T) {
	si := GetMOEXSpec("Si85000BR5", 85000, false)
	if si.Underlying != "Si" || si.Multiplier != 1.0 || si.TickSize != 1.0 {
		t.Fatalf("unexpected Si spec: %+v", si)
	}

	ri := GetMOEXSpec("RI85000BR5", 85000, true)
	if ri.Underlying != "RI" || ri.Multiplier != 2.0 || ri.TickSize != 10.0 {
		t.Fatalf("unexpected RI spec: %+v", ri)
	}

	if !ri.IsCall {
		t.Fatal("RI spec should be a call")
	}
}

// TestCalculateMOEXParitySpread verifies thresholds and rounding.
func TestCalculateMOEXParitySpread(t *testing.T) {
	spot, strike, days, keyRate := 83200.0, 83000.0, 30.0, 0.16
	tm := days / 365.0
	theoDiff := spot - strike*math.Exp(-keyRate*tm)

	// Fair -> No Arbitrage.
	_, _, strategy := CalculateMOEXParitySpread(spot, strike, days, theoDiff+100, 100, keyRate)
	if strategy != "No Arbitrage" {
		t.Fatalf("fair chain should be No Arbitrage, got %s", strategy)
	}

	// Call overpriced by 50 (> threshold 30) -> Conversion.
	_, _, strategy = CalculateMOEXParitySpread(spot, strike, days, theoDiff+100+50, 100, keyRate)
	if strategy != "Conversion (Sell Call, Buy Put & Underlying Futures)" {
		t.Fatalf("expected Conversion, got %s", strategy)
	}

	// Put overpriced by 50 -> Reversal.
	_, _, strategy = CalculateMOEXParitySpread(spot, strike, days, theoDiff+100, 100+50, keyRate)
	if strategy != "Reversal (Buy Call, Sell Put & Short Underlying Futures)" {
		t.Fatalf("expected Reversal, got %s", strategy)
	}
}