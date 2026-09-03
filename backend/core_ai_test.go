package main

import (
	"math"
	"strings"
	"testing"
)

func TestSpreadsAlreadyOpen(t *testing.T) {
	open := []spreadRecord{
		{Symbol: "Si", Type: "bull_put", ShortStrike: 86500, LongStrike: 86000},
		{Symbol: "Si", Type: "bear_call", ShortStrike: 88000, LongStrike: 88500},
	}
	p := &spreadPlan{Symbol: "Si", Type: "bull_put", ShortStrike: 86500, LongStrike: 86000}
	if !spreadAlreadyOpen(p, open) {
		t.Error("identical strike pair on same symbol/type should be considered open")
	}
	p2 := &spreadPlan{Symbol: "Si", Type: "bull_put", ShortStrike: 87000, LongStrike: 86500}
	if spreadAlreadyOpen(p2, open) {
		t.Error("different strikes are a different trade")
	}
	p3 := &spreadPlan{Symbol: "Si", Type: "bull_call", ShortStrike: 86500, LongStrike: 86000}
	if spreadAlreadyOpen(p3, open) {
		t.Error("different construction type is not the same trade")
	}
	p4 := &spreadPlan{Symbol: "CR", Type: "bull_put", ShortStrike: 86500, LongStrike: 86000}
	if spreadAlreadyOpen(p4, open) {
		t.Error("different instrument is not the same trade")
	}
	if !spreadAlreadyOpen(&spreadPlan{Symbol: "Si", Type: "bear_call", ShortStrike: 88000.4, LongStrike: 88500.4}, open) {
		t.Error("strikes within 0.5 tolerance should match")
	}
}

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

func TestPopScoreAdjustBands(t *testing.T) {
	cases := []struct {
		pop     int
		adj     int
		hasLine bool
	}{
		{100, 8, true},
		{70, 8, true},
		{69, 4, false},
		{55, 4, false},
		{54, 0, false},
		{40, 0, false},
		{39, -4, false},
		{30, -4, false},
		{29, -8, true},
		{0, -8, true},
	}
	for _, tc := range cases {
		adj, line := popScoreAdjust(tc.pop)
		if adj != tc.adj {
			t.Fatalf("popScoreAdjust(%d) adj = %d, want %d", tc.pop, adj, tc.adj)
		}
		if (line != "") != tc.hasLine {
			t.Fatalf("popScoreAdjust(%d) line %q, want line=%v", tc.pop, line, tc.hasLine)
		}
	}
}

// TestCandidatePopWeightMovesScore builds the same credit plan twice with only
// the entry spot moved to opposite extremes: Monte-Carlo PoP lands at ~100 vs
// ~0, so the scores must differ by exactly the +8/−8 band swing. The extremes
// sit ~5σ out, so the MC outcome is practically deterministic.
func TestCandidatePopWeightMovesScore(t *testing.T) {
	plan := &spreadPlan{
		Symbol: "Si", Type: "bull_put", DisplayName: "Bull Put Spread",
		Expiry: "2026-09-24", DaysToExp: 20, Spot: 60000,
		ShortStrike: 86000, LongStrike: 87000, WingWidth: 1000,
		NetCredit: 100, MaxProfit: 100, MaxLoss: 400, Qty: 1,
	}
	in := coreInstrument{
		Symbol: "Si", Spot: 60000, Regime: "BULLISH", Strength: "rising",
		IVATM: 30, HV20: 25, ATR14: 0, LiquidityPct: 5,
	}
	hi := candidateFromPlan(plan, in)
	if hi.PopProb < 70 {
		t.Fatalf("high-pop setup PopProb = %d, want >= 70", hi.PopProb)
	}
	in.Spot = 120000
	lo := candidateFromPlan(plan, in)
	if lo.PopProb >= 30 {
		t.Fatalf("low-pop setup PopProb = %d, want < 30", lo.PopProb)
	}
	if d := hi.Score - lo.Score; d != 16 {
		t.Fatalf("score swing = %d, want 16 (+8 vs −8)", d)
	}
	foundHi, foundLo := false, false
	for _, r := range hi.Reasons {
		if strings.Contains(r, "высокий") {
			foundHi = true
		}
	}
	for _, r := range lo.Reasons {
		if strings.Contains(r, "низкий") {
			foundLo = true
		}
	}
	if !foundHi {
		t.Fatalf("high-pop reasons missing PoP line: %v", hi.Reasons)
	}
	if !foundLo {
		t.Fatalf("low-pop reasons missing PoP flag: %v", lo.Reasons)
	}
}
