package main

import (
	"math"
	"testing"
)

func adviceTestBase() adviceInputs {
	return adviceInputs{
		spreadType: "bull_put", IsDebit: false,
		DTE: 30, CreditPerShare: 4, Width: 10,
		IvATM: 0.35, Hv20: 0.28, IvRankAvail: true, IvRank: 60,
		TrendRegime: "BULLISH", TrendStrength: "rising",
		LiquidityPct: 8,
	}
}

func runFullScoring(in adviceInputs) *spreadAdvice {
	adv := scoreSpreadAdvice(in)
	scoreTrendFit(in, func(id, title, status, detail string, earned, weight int) {
		adv.Checks = append(adv.Checks, adviceCheck{ID: id, Title: title, Status: status, Detail: detail, Earned: earned, Weight: weight})
	})
	total := 0
	for _, c := range adv.Checks {
		total += c.Earned
	}
	adv.Score = total
	if total >= 70 {
		adv.Verdict = "РЕКОМЕНДУЕТСЯ"
	} else if total >= 45 {
		adv.Verdict = "ОСТОРОЖНО"
	} else {
		adv.Verdict = "НЕ РЕКОМЕНДУЕТСЯ"
	}
	return adv
}

func TestScoreStrongCreditBullSetup(t *testing.T) {
	in := adviceTestBase()
	// bull_put is a credit spread; bullish trend fits.
	adv := runFullScoring(in)
	if adv.Verdict != "РЕКОМЕНДУЕТСЯ" {
		t.Fatalf("verdict = %q (%d), want РЕКОМЕНДУЕТСЯ", adv.Verdict, adv.Score)
	}
	for _, c := range adv.Checks {
		if c.Status == "bad" || c.Status == "warn" {
			t.Fatalf("check %s has status %s in an ideal setup", c.ID, c.Status)
		}
	}
}

func TestScoreWeakDebitAgainstTrend(t *testing.T) {
	in := adviceInputs{
		spreadType: "bull_call", IsDebit: true,
		DTE: 5, CreditPerShare: -2, Width: 10, MaxProfit: 8, MaxLoss: 10,
		IvATM: 0.40, Hv20: 0.30, // expensive premium for a debit
		TrendRegime: "BEARISH", TrendStrength: "falling", // против бычьего дебета
		LiquidityPct: 40,
	}
	adv := runFullScoring(in)
	if adv.Verdict != "НЕ РЕКОМЕНДУЕТСЯ" {
		t.Fatalf("verdict = %q (%d), want НЕ РЕКОМЕНДУЕТСЯ", adv.Verdict, adv.Score)
	}
}

func TestScoreUnknownMarketDataStaysCautious(t *testing.T) {
	in := adviceInputs{
		spreadType: "bull_put", IsDebit: false,
		DTE: 30, CreditPerShare: 3.5, Width: 10,
		LiquidityPct: -1, // no quotes
		// no IV/HV, no trend
	}
	adv := runFullScoring(in)
	if adv.Verdict != "ОСТОРОЖНО" && adv.Verdict != "НЕ РЕКОМЕНДУЕТСЯ" {
		t.Fatalf("verdict = %q without market data, want cautious", adv.Verdict)
	}
	for _, c := range adv.Checks {
		if (c.ID == "vol" || c.ID == "trend") && c.Status != "info" {
			t.Fatalf("missing data must yield info status, %s=%s", c.ID, c.Status)
		}
	}
}

func TestHVAndTrendStatsPure(t *testing.T) {
	closes := make([]float64, 60)
	for i := range closes {
		closes[i] = 100 + float64(i)*0.5 // steady rise 100..129.5
	}
	ts := computeTrendStats(closes)
	if ts.Regime != "BULLISH" {
		t.Fatalf("regime = %q, want BULLISH", ts.Regime)
	}
	if ts.Strength != "rising" {
		t.Fatalf("strength = %q, want rising", ts.Strength)
	}
	if ts.RSI14 < 70 {
		t.Fatalf("rsi = %.1f, want overbought on a straight rally", ts.RSI14)
	}
	hv := hvFromCloses(closes[len(closes)-21:], 20)
	if hv <= 0 || hv > math.MaxFloat64 {
		t.Fatalf("hv = %v out of range", hv)
	}
}
