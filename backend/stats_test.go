package main

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"option-quant-ai/quant"
)

func statsTestTrades() []quant.Trade {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mk := func(id, strat, sym string, pnl float64, day int, trend, vol string, dte int) quant.Trade {
		return quant.Trade{
			ID: id, Strategy: strat, Symbol: sym,
			OpenedAt: base.Add(time.Duration(day) * 24 * time.Hour),
			ClosedAt: base.Add(time.Duration(day+3) * 24 * time.Hour),
			RealizedPnL: pnl,
			TrendAtEntry: trend, VolRegime: vol, DTEAtEntry: dte,
		}
	}
	return []quant.Trade{
		mk("t1", "Bull Put Spread", "Si", 500, 1, "BULLISH", "IV>HV", 25),
		mk("t2", "Bull Put Spread", "Si", -300, 4, "BULLISH", "IV>HV", 30),
		mk("t3", "Bear Call Spread", "Si", 200, 7, "BEARISH", "IV>HV", 18),
		mk("t4", "Bear Call Spread", "SBERP", -100, 10, "BEARISH", "IV<HV", 40),
		mk("t5", "Bull Put Spread", "Si", 700, 13, "SIDEWAYS", "IV>HV", 10),
	}
}

func TestStatsOverviewAggregates(t *testing.T) {
	o := computeStatsOverview(statsTestTrades())
	if o.Trades != 5 {
		t.Fatalf("trades = %d", o.Trades)
	}
	if o.NetPnl != 1000 {
		t.Fatalf("net pnl = %v, want 1000", o.NetPnl)
	}
	if o.Wins != 3 || o.Losses != 2 {
		t.Fatalf("wins/losses = %d/%d", o.Wins, o.Losses)
	}
	if o.WinRate != 0.6 {
		t.Fatalf("win rate = %v", o.WinRate)
	}
	if o.MaxDrawdown >= 0 {
		t.Fatalf("max dd = %v, want negative", o.MaxDrawdown)
	}
	if len(o.Equity) != 5 || len(o.Monthly) == 0 || len(o.Histogram) == 0 {
		t.Fatalf("series lengths: eq=%d monthly=%d hist=%d", len(o.Equity), len(o.Monthly), len(o.Histogram))
	}
}

func TestStatsBreakdownByStrategy(t *testing.T) {
	rows := computeBreakdown(statsTestTrades(), func(t quant.Trade) string { return t.Strategy })
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Bull Put: 500-300+700=900 > Bear Call: 200-100=100 → sorted first.
	if rows[0].Key != "Bull Put Spread" || rows[0].NetPnl != 900 {
		t.Fatalf("top row = %+v", rows[0])
	}
	if math.Abs(rows[0].WinRate-2.0/3.0) > 0.01 {
		t.Fatalf("win rate = %v", rows[0].WinRate)
	}
}

func TestMCFanDeterministicAndSane(t *testing.T) {
	pnls := []float64{100, -50, 200, -80, 50}
	a := mcFan(pnls, 50, 500, rand.New(rand.NewSource(7)))
	b := mcFan(pnls, 50, 500, rand.New(rand.NewSource(7)))
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("fan lengths %d/%d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("not deterministic at %d", i)
		}
		if a[i].P5 > a[i].P50 || a[i].P50 > a[i].P95 {
			t.Fatalf("percentiles out of order at step %d", a[i].Step)
		}
	}
	p := probProfitMC(pnls, 50, 500, rand.New(rand.NewSource(7)))
	if p < 0 || p > 1 {
		t.Fatalf("prob profit = %v", p)
	}
}

func TestStrategyForecastOrdering(t *testing.T) {
	fs := buildStrategyForecasts(statsTestTrades())
	if len(fs) != 2 {
		t.Fatalf("strategies = %d", len(fs))
	}
	if fs[0].Key != "Bull Put Spread" {
		t.Fatalf("best = %s", fs[0].Key)
	}
	if fs[0].TStat == 0 {
		t.Fatalf("t-stat not computed")
	}
}
