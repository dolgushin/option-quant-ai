package quant

import (
	"math"
	"testing"
	"time"
)

func resetState() {
	SetDataFile("")
	SetPositions(nil)
	ClearTrades()
	SetInitialCapital(1000000)
}

// TestNetFuturesLegs pins FIFO netting: opposite same-contract legs cancel,
// realized moves to the accumulator at the exit price, residual opens.
func TestNetFuturesLegs(t *testing.T) {
	mk := func() []PositionLeg {
		return []PositionLeg{
			{SecID: "SiZ6", Kind: "FUTURES", Side: "BUY", Quantity: 2, EntryPrice: 86000},
			{SecID: "SiZ6", Kind: "FUTURES", Side: "BUY", Quantity: 1, EntryPrice: 86100},
			{SecID: "SiU6", Kind: "FUTURES", Side: "BUY", Quantity: 5, EntryPrice: 85000},
		}
	}
	// Sell 2 against two BUY lots: full net, realized at exit 86200.
	legs, realized, residual := NetFuturesLegs(mk(), "SiZ6", "SELL", 2, 86200, 1)
	if residual != 0 {
		t.Fatalf("residual = %d, want 0 (fully netted)", residual)
	}
	// Lot1: +(86200-86000)*2 = 400; lot2 untouched (FIFO took lot1 fully? no:
	// 2 requested, lot1 has 2 → lot1 closes fully, lot2 stays).
	if realized != 400 {
		t.Fatalf("realized = %v, want 400", realized)
	}
	if len(legs) != 2 || legs[0].Quantity != 1 {
		t.Fatalf("legs wrong after netting: %+v", legs)
	}
	// Sell 5: closes lot1 (2) + lot2 (1), residual 2 to open.
	legs, realized, residual = NetFuturesLegs(mk(), "SiZ6", "SELL", 5, 86200, 1)
	if residual != 2 {
		t.Fatalf("residual = %d, want 2", residual)
	}
	// 400 + (86200-86100)*1 = 500.
	if realized != 500 {
		t.Fatalf("realized = %v, want 500", realized)
	}
	// Different contract untouched; same-side adds (no netting within call).
	legs, realized, residual = NetFuturesLegs(mk(), "SiZ6", "BUY", 3, 86200, 1)
	if realized != 0 || residual != 3 || len(legs) != 3 {
		t.Fatalf("same-side must pass through: %+v %v %d", legs, realized, residual)
	}
	// Options never net.
	opt := []PositionLeg{{SecID: "X", Kind: "OPTION", Side: "SELL", Quantity: 2, EntryPrice: 100}}
	legs, realized, residual = NetFuturesLegs(opt, "X", "BUY", 2, 90, 1)
	if realized != 0 || residual != 2 || len(legs) != 1 {
		t.Fatalf("options must not net: %+v %v %d", legs, realized, residual)
	}
	// Mult scales realized.
	_, realized, _ = NetFuturesLegs(mk(), "SiZ6", "SELL", 2, 86200, 100)
	if realized != 40000 {
		t.Fatalf("mult-scaled realized = %v, want 40000", realized)
	}
}

// TestSettleTradeFoldsRealized: journaled P&L = live + netted, counted once.
func TestSettleTradeFoldsRealized(t *testing.T) {
	p := Position{Strategy: "S", Symbol: "Si", EntryValue: -1000, CurrentValue: -600,
		PnL: 400, RealizedPnL: 150}
	tr := SettleTrade(p)
	if tr.RealizedPnL != 550 {
		t.Fatalf("settled = %v, want 550", tr.RealizedPnL)
	}
	if math.Abs(tr.PnLPercent-55) > 1e-9 {
		t.Fatalf("percent = %v, want 55", tr.PnLPercent)
	}
	plain := Position{Strategy: "S", Symbol: "Si", EntryValue: -1000, CurrentValue: -600, PnL: 400}
	tr2 := SettleTrade(plain)
	if tr2.RealizedPnL != 400 || math.Abs(tr2.PnLPercent-40) > 1e-9 {
		t.Fatalf("plain settle = %+v, want 400/40", tr2)
	}
}

// TestAddTradeAndStats verifies statistics over a win/loss trade set.
func TestAddTradeAndStats(t *testing.T) {
	resetState()

	AddTrade(Trade{ID: "t1", Strategy: "IC", Symbol: "Si", OpenedAt: time.Now(), ClosedAt: time.Now(), EntryValue: 1000, ExitValue: 1500, RealizedPnL: 500, PnLPercent: 50})
	AddTrade(Trade{ID: "t2", Strategy: "IC", Symbol: "Si", OpenedAt: time.Now(), ClosedAt: time.Now(), EntryValue: 1000, ExitValue: 500, RealizedPnL: -500, PnLPercent: -50})
	AddTrade(Trade{ID: "t3", Strategy: "BP", Symbol: "Si", OpenedAt: time.Now(), ClosedAt: time.Now(), EntryValue: 2000, ExitValue: 2500, RealizedPnL: 500, PnLPercent: 25})

	stats := ComputeStats()
	if stats.TotalTrades != 3 {
		t.Fatalf("total trades=%d, want 3", stats.TotalTrades)
	}
	if stats.WinningTrades != 2 || stats.LosingTrades != 1 {
		t.Fatalf("W/L=%d/%d, want 2/1", stats.WinningTrades, stats.LosingTrades)
	}
	if stats.TotalRealizedPnL != 500 {
		t.Fatalf("total realized PnL=%.0f, want 500", stats.TotalRealizedPnL)
	}
	if stats.AvgWin != 500 || stats.AvgLoss != -500 {
		t.Fatalf("avgWin=%.0f avgLoss=%.0f, want 500/-500", stats.AvgWin, stats.AvgLoss)
	}
	wantRate := 100.0 * 2 / 3
	if math.Abs(stats.WinRate-wantRate) > 0.001 {
		t.Fatalf("win rate=%.4f, want %.4f", stats.WinRate, wantRate)
	}
	if stats.BestTrade != 500 || stats.WorstTrade != -500 {
		t.Fatalf("best=%.0f worst=%.0f, want 500/-500", stats.BestTrade, stats.WorstTrade)
	}
	if stats.ProfitFactor != 2.0 {
		t.Fatalf("profit factor=%.2f, want 2.0 (winTotal=1000 / lossTotal=500)", stats.ProfitFactor)
	}
}

// TestProfitFactorNoLosses verifies infinite profit factor when no losing trades.
func TestProfitFactorNoLosses(t *testing.T) {
	resetState()
	AddTrade(Trade{ID: "t1", Strategy: "IC", Symbol: "Si", OpenedAt: time.Now(), ClosedAt: time.Now(), EntryValue: 1000, ExitValue: 1500, RealizedPnL: 500, PnLPercent: 50})

	stats := ComputeStats()
	if stats.ProfitFactor != 99999.0 {
		t.Fatalf("profit factor=%.0f, want 99999 (infinite)", stats.ProfitFactor)
	}
}

// TestClearTrades verifies history removal and persistence safety.
func TestClearTrades(t *testing.T) {
	resetState()
	AddTrade(Trade{ID: "t1", Strategy: "IC", Symbol: "Si", OpenedAt: time.Now(), ClosedAt: time.Now(), EntryValue: 1000, ExitValue: 1500, RealizedPnL: 500, PnLPercent: 50})

	if n := ClearTrades(); n != 1 {
		t.Fatalf("cleared %d trades, want 1", n)
	}
	if len(GetTrades()) != 0 {
		t.Fatal("trade history should be empty after ClearTrades")
	}
	stats := ComputeStats()
	if stats.TotalTrades != 0 || stats.TotalRealizedPnL != 0 {
		t.Fatalf("stats should reset, got %+v", stats)
	}
}

// TestSaveAndRemovePosition verifies add/replace/remove position semantics.
func TestRemoveTradesByStrategy(t *testing.T) {
	resetState()
	AddTrade(Trade{ID: "a", Strategy: "Protective Grid", RealizedPnL: 100})
	AddTrade(Trade{ID: "b", Strategy: "Short Straddle", RealizedPnL: 50})
	AddTrade(Trade{ID: "c", Strategy: "Protective Grid", RealizedPnL: -20})
	if n := RemoveTradesByStrategy("Protective Grid"); n != 2 {
		t.Fatalf("want 2 removed, got %d", n)
	}
	left := GetTrades()
	if len(left) != 1 || left[0].ID != "b" {
		t.Fatalf("only other strategies must survive: %+v", left)
	}
	if n := RemoveTradesByStrategy("Protective Grid"); n != 0 {
		t.Fatalf("second wipe must remove 0, got %d", n)
	}
	resetState()
}

func TestSaveAndRemovePosition(t *testing.T) {
	resetState()

	p1 := Position{ID: "p1", Strategy: "IC", Symbol: "Si", PnL: 100}
	p2 := Position{ID: "p2", Strategy: "BP", Symbol: "RI", PnL: -50}

	SavePosition(p1)
	SavePosition(p2)

	if len(GetActivePositions()) != 2 {
		t.Fatalf("expected 2 positions, got %d", len(GetActivePositions()))
	}

	// Replace p1.
	p1.PnL = 200
	SavePosition(p1)
	positions := GetActivePositions()
	if len(positions) != 2 {
		t.Fatalf("replacing should not add duplicate, got %d", len(positions))
	}
	for _, p := range positions {
		if p.ID == "p1" && p.PnL != 200 {
			t.Fatalf("p1 not replaced, PnL=%.0f", p.PnL)
		}
	}

	removed, ok := RemovePosition("p1")
	if !ok || removed.ID != "p1" {
		t.Fatalf("RemovePosition failed: ok=%v removed=%+v", ok, removed)
	}
	if len(GetActivePositions()) != 1 {
		t.Fatalf("expected 1 position after removal, got %d", len(GetActivePositions()))
	}

	if _, ok := RemovePosition("nonexistent"); ok {
		t.Fatal("removing missing position should return found=false")
	}
}

// TestGetPortfolio verifies unrealized PnL is aggregated across positions.
func TestGetPortfolio(t *testing.T) {
	resetState()
	SetInitialCapital(1000000)

	SavePosition(Position{ID: "p1", Strategy: "IC", Symbol: "Si", PnL: 5000, Margin: 25000})
	SavePosition(Position{ID: "p2", Strategy: "BP", Symbol: "RI", PnL: -2000, Margin: 15000})

	port := GetPortfolio()
	if port.InitialCapital != 1000000 {
		t.Fatalf("initial capital=%.0f", port.InitialCapital)
	}
	if port.UnrealizedPnL != 3000 {
		t.Fatalf("unrealized PnL=%.0f, want 3000", port.UnrealizedPnL)
	}
	if port.LockedMargin != 40000 {
		t.Fatalf("locked margin=%.0f, want 40000", port.LockedMargin)
	}
	if port.TotalValue != 1003000 {
		t.Fatalf("total value=%.0f, want 1003000", port.TotalValue)
	}
	wantCash := 1000000.0 - 40000.0 + 3000.0
	if port.Cash != wantCash {
		t.Fatalf("cash=%.0f, want %.0f", port.Cash, wantCash)
	}
}
