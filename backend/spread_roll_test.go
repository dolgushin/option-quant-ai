package main

import (
	"testing"
)

func buildRollTestPlan(t *testing.T, qty int) *spreadPlan {
	t.Helper()
	legs := []rollLegSpec{
		{Side: "SELL", IsCall: true, CurrentStrike: 80000, TargetStrike: 81000},
		{Side: "BUY", IsCall: true, CurrentStrike: 82500, TargetStrike: 82500},
	}
	plan, err := buildSpreadFromLegs("RI", "2026-10-15", qty, legs, false, "bear_call", "Bear Call Spread")
	if err != nil {
		t.Skipf("network/chain unavailable: %v", err)
	}
	return plan
}

func TestBuildSpreadFromLegsEconomics(t *testing.T) {
	// Bear call roll: SELL 81000C, BUY 82500C (credit), single lot.
	plan := buildRollTestPlan(t, 1)
	if plan.ShortStrike != 81000 {
		t.Fatalf("short strike = %v, want 81000", plan.ShortStrike)
	}
	if plan.LongStrike != 82500 {
		t.Fatalf("long strike = %v, want 82500", plan.LongStrike)
	}
	width := 82500.0 - 81000.0
	if plan.WingWidth != width {
		t.Fatalf("wing width = %v, want %v", plan.WingWidth, width)
	}
	// Net credit must be > 0 (credit) and < wing width (sanity).
	if plan.NetCredit <= 0 || plan.NetCredit >= width {
		t.Fatalf("net credit = %v out of (0, %v)", plan.NetCredit, width)
	}
	// maxProfit = netCredit, maxLoss = wing - netCredit (credit structure).
	if plan.MaxProfit != plan.NetCredit {
		t.Fatalf("max profit = %v, want net credit %v", plan.MaxProfit, plan.NetCredit)
	}
	expectedLoss := width - plan.NetCredit
	if plan.MaxLoss != expectedLoss {
		t.Fatalf("max loss = %v, want %v", plan.MaxLoss, expectedLoss)
	}
	if len(plan.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(plan.Legs))
	}
	if plan.Legs[0].Side != "SELL" || plan.Legs[1].Side != "BUY" {
		t.Fatalf("unexpected leg sides: %v, %v", plan.Legs[0].Side, plan.Legs[1].Side)
	}
}

func TestBuildSpreadFromLegsScalesByQty(t *testing.T) {
	one := buildRollTestPlan(t, 1)
	five := buildRollTestPlan(t, 5)
	if five.NetCredit != one.NetCredit*5 {
		t.Fatalf("net credit scaling: %v vs %v×5", five.NetCredit, one.NetCredit)
	}
	if five.MaxLoss != one.MaxLoss*5 {
		t.Fatalf("max loss scaling: %v vs %v×5", five.MaxLoss, one.MaxLoss)
	}
}

