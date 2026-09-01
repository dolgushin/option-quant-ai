package main

import (
	"math"
	"testing"
)

func TestRecomputePlanEconomics(t *testing.T) {
	// Credit bull-put: SELL 86500 @ 100, BUY 86000 @ 60, wing 500, qty 2.
	plan := &spreadPlan{
		Symbol: "Si", ShortStrike: 86500, LongStrike: 86000, WingWidth: 500,
		Qty: 2, IsDebit: false,
		Legs: []spreadLeg{
			{Side: "SELL", Strike: 86500, Price: 100, MarginShort: 300},
			{Side: "BUY", Strike: 86000, Price: 60},
		},
	}
	recomputePlanEconomics(plan)
	if want := 80.0; math.Abs(plan.NetCredit-want) > 0.001 {
		t.Errorf("NetCredit = %.2f, want %.2f (credit 100-60 = 40/contract × qty 2)", plan.NetCredit, want)
	}
	if want := 80.0; math.Abs(plan.MaxProfit-want) > 0.001 {
		t.Errorf("MaxProfit = %.2f, want %.2f", plan.MaxProfit, want)
	}
	if want := 920.0; math.Abs(plan.MaxLoss-want) > 0.001 {
		t.Errorf("MaxLoss = %.2f, want %.2f (wing 500 - credit 40) × 2", plan.MaxLoss, want)
	}
	if want := 600.0; math.Abs(plan.MarginShort-want) > 0.001 {
		t.Errorf("MarginShort = %.2f, want %.2f", plan.MarginShort, want)
	}
}

func TestRecomputePlanEconomicsDebit(t *testing.T) {
	// Debit call spread: BUY 86500 @ 120, SELL 87000 @ 60, wing 500, qty 1.
	plan := &spreadPlan{
		Symbol: "Si", ShortStrike: 87000, LongStrike: 86500, WingWidth: 500,
		Qty: 1, IsDebit: true,
		Legs: []spreadLeg{
			{Side: "SELL", Strike: 87000, Price: 60, MarginShort: 400},
			{Side: "BUY", Strike: 86500, Price: 120},
		},
	}
	recomputePlanEconomics(plan)
	if want := -60.0; math.Abs(plan.NetCredit-want) > 0.001 {
		t.Errorf("NetCredit = %.2f, want %.2f (−60 net debit)", plan.NetCredit, want)
	}
	if want := 440.0; math.Abs(plan.MaxProfit-want) > 0.001 {
		t.Errorf("MaxProfit = %.2f, want %.2f (wing 500 − debit 60)", plan.MaxProfit, want)
	}
	if want := 60.0; math.Abs(plan.MaxLoss-want) > 0.001 {
		t.Errorf("MaxLoss = %.2f, want %.2f", plan.MaxLoss, want)
	}
}

func TestInAiQuietHours(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{22, true},
		{23, true},
		{0, true},
		{5, true},
		{8, true},
		{9, false},
		{10, false},
		{12, false},
		{17, false},
		{20, false},
		{21, false},
	}
	for _, c := range cases {
		if got := inAiQuietHours(c.hour); got != c.want {
			t.Errorf("inAiQuietHours(%d) = %v, want %v", c.hour, got, c.want)
		}
	}
}

func TestInAiQuietHoursBoundaries(t *testing.T) {
	if !inAiQuietHours(22) {
		t.Error("22:00 должен быть тихим часом")
	}
	if !inAiQuietHours(8) {
		t.Error("08:59 должен быть тихим часом")
	}
	if inAiQuietHours(9) {
		t.Error("09:00 должен быть рабочим часом")
	}
	if inAiQuietHours(21) {
		t.Error("21:59 должен быть рабочим часом")
	}
}