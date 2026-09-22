package main

import (
	"math"
	"testing"

	"option-quant-ai/quant"
)

func TestProtectGridSignalExhaustedLong(t *testing.T) {
	// Down day: open 86000, low 84500, last 84600, ATR 1200 → drift 1400/1200 > 90%.
	sig := protectGridSignal(86000, 86100, 84500, 84600, 1200)
	if !sig.Ready || sig.Direction != gridLong || sig.Tier != gridTierExhausted {
		t.Fatalf("want READY LONG EXHAUSTED, got %+v", sig)
	}
}

func TestProtectGridSignalHalfShort(t *testing.T) {
	// Up day half move: range 700/1200 = 58%, drift 600/1200 = 50%.
	sig := protectGridSignal(85000, 85650, 84950, 85600, 1200)
	if sig.Ready || sig.Direction != gridShort || sig.Tier != gridTierHalf {
		t.Fatalf("want scout SHORT HALF, got %+v", sig)
	}
}

func TestProtectGridSignalFlatNone(t *testing.T) {
	sig := protectGridSignal(85000, 85100, 84950, 85020, 1200)
	if sig.Ready || sig.Direction != "" {
		t.Fatalf("want no signal on 2%% ATR day, got %+v", sig)
	}
}

func TestProtectGridSignalBadATR(t *testing.T) {
	sig := protectGridSignal(85000, 85100, 84900, 85000, 0)
	if sig.Ready {
		t.Fatalf("no ATR must not signal: %+v", sig)
	}
}

func TestResolveProtectStrikeOneStep(t *testing.T) {
	strikes := []float64{84000, 84500, 85000, 85500, 86000}
	if s := resolveProtectStrike(strikes, 85100, gridLong); s != 85000 {
		t.Fatalf("long put must sit 1 step below spot, got %v", s)
	}
	if s := resolveProtectStrike(strikes, 85100, gridShort); s != 85500 {
		t.Fatalf("short call must sit 1 step above spot, got %v", s)
	}
}

func TestGridLadderTarget(t *testing.T) {
	if got := gridLadderTarget(gridLong, 86000, 86000, 25, 5); got != 1 {
		t.Fatalf("at entry ladder holds first unit, got %d", got)
	}
	if got := gridLadderTarget(gridLong, 86000, 85940, 25, 5); got != 3 {
		t.Fatalf("60pts under entry = 3 units, got %d", got)
	}
	if got := gridLadderTarget(gridLong, 86000, 85000, 25, 5); got != 5 {
		t.Fatalf("must clamp at max, got %d", got)
	}
	if got := gridLadderTarget(gridShort, 86000, 86060, 25, 4); got != 3 {
		t.Fatalf("short ladder mirrored, got %d", got)
	}
	if got := gridLadderTarget("SIDEWAYS", 86000, 85900, 25, 4); got != 0 {
		t.Fatalf("unknown direction → 0, got %d", got)
	}
}

func TestGridBreakeven(t *testing.T) {
	// theta 300 ₽/day, step 25×1 − fee 4 → edge 21 → 15 fills.
	if got := gridBreakevenFillsPerDay(300, 25, 1, 4); got != 15 {
		t.Fatalf("want 15 fills, got %v", got)
	}
	if got := gridBreakevenFillsPerDay(300, 10, 1, 12); !math.IsInf(got, 1) {
		t.Fatalf("negative edge must be Inf, got %v", got)
	}
}

func TestSimulateGridChopEarns(t *testing.T) {
	path := synthPath("chop", 86000, 160, 150)
	real, _, fills, _, _ := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 5)
	if fills == 0 || real <= 0 {
		t.Fatalf("chop must earn without fees: real=%v fills=%d", real, fills)
	}
}

func TestSimulateGridFlatNoFillsWeird(t *testing.T) {
	path := synthPath("flat", 86000, 40, 0)
	real, _, fills, inv, _ := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 5)
	// Flat at entry: ladder opens the first unit once, no take profits.
	if inv != 1 || fills != 1 || real != 0 {
		t.Fatalf("flat holds 1 unit idle: real=%v fills=%d inv=%d", real, fills, inv)
	}
}

func TestSimulateGridTrendAgainstCapsInventory(t *testing.T) {
	path := synthPath("trend_down", 86000, 160, 2000)
	_, unreal, _, inv, adv := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 5)
	if inv != 5 {
		t.Fatalf("inventory must clamp at max 5, got %d", inv)
	}
	if unreal >= 0 || adv <= 0 {
		t.Fatalf("trend against a LONG grid must sit in adverse water: unreal=%v adv=%d", unreal, adv)
	}
}

func TestSimulateGridTrendWithUs(t *testing.T) {
	// LONG grid into an up-trend: first unit takes profit, ladder idles.
	path := synthPath("trend_up", 86000, 160, 500)
	real, _, fills, inv, _ := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 5)
	if real <= 0 || fills == 0 {
		t.Fatalf("trend with us must bank the first unit: real=%v fills=%d inv=%d", real, fills, inv)
	}
}

func TestGridTakeProfitLegs(t *testing.T) {
	legs := []quant.PositionLeg{
		{Kind: "FUTURES", Side: "BUY", Quantity: 2, EntryPrice: 85900},
		{Kind: "FUTURES", Side: "BUY", Quantity: 1, EntryPrice: 85990},
	}
	if q := gridTakeProfitLegs(legs, gridLong, 85930, 25); q != 2 {
		t.Fatalf("only the 85900 leg touched +25, got %d", q)
	}
	if q := gridTakeProfitLegs(legs, gridShort, 85930, 25); q != 0 {
		t.Fatalf("short TP must not fire on long legs, got %d", q)
	}
}

func TestGridOpenInventory(t *testing.T) {
	legs := []quant.PositionLeg{
		{Kind: "FUTURES", Side: "BUY", Quantity: 3},
		{Kind: "FUTURES", Side: "SELL", Quantity: 1},
		{Kind: "OPTION", Side: "BUY", Quantity: 10},
	}
	if n := gridOpenInventory(legs, gridLong); n != 2 {
		t.Fatalf("want net 2 longs, got %d", n)
	}
	if n := gridOpenInventory(legs, gridShort); n != 0 {
		t.Fatalf("mirrored count must not go negative, got %d", n)
	}
}

func TestEvaluateProtectGridStops(t *testing.T) {
	g := &protectGridRecord{MaxLossRub: 5000, ProfitTarget: 3000, TimeStopDTE: 7}
	if ev := evaluateProtectGrid(g, -6000, 20); ev.Action != "CLOSE_STOP" {
		t.Fatalf("want CLOSE_STOP, got %+v", ev)
	}
	if ev := evaluateProtectGrid(g, 3500, 20); ev.Action != "CLOSE_TP" {
		t.Fatalf("want CLOSE_TP, got %+v", ev)
	}
	if ev := evaluateProtectGrid(g, 100, 5); ev.Action != "CLOSE_TIME" {
		t.Fatalf("want CLOSE_TIME, got %+v", ev)
	}
	if ev := evaluateProtectGrid(g, 100, 20); ev.Action != "NONE" {
		t.Fatalf("want NONE, got %+v", ev)
	}
}
