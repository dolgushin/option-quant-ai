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