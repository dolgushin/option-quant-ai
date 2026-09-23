package main

import (
	"math"
	"testing"
	"time"

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
	// theta 300 ₽/day, one round trip earns the 25-point TAKE (×1) and pays
	// 2×4 fee → edge 17 → 18 round trips.
	if got := gridBreakevenRoundTripsPerDay(300, 25, 1, 4); got != 18 {
		t.Fatalf("want 18 round trips, got %v", got)
	}
	if got := gridBreakevenRoundTripsPerDay(300, 10, 1, 12); !math.IsInf(got, 1) {
		t.Fatalf("take below two fees must be Inf, got %v", got)
	}
	if finiteOrNil(math.Inf(1)) != nil {
		t.Fatalf("Inf breakeven must encode as null")
	}
	if v := finiteOrNil(18); v == nil || *v != 18 {
		t.Fatalf("finite breakeven must survive, got %v", v)
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

func TestAggregateProtectGridStats(t *testing.T) {
	recs := []protectGridRecord{
		{ID: "g1", Symbol: "Si", Direction: gridLong, Status: "CLOSED",
			OpenedAt: "2026-09-20T10:00:00Z", ClosedAt: "2026-09-21T10:00:00Z",
			FinalPnl: 1500, Fills: 6, RealizedGrid: 900, FeePerFill: 4,
			FillLog: []gridFill{
				{Kind: "TAKE", ClosedPnl: 21}, {Kind: "TAKE", ClosedPnl: 21}, {Kind: "LADDER"},
			}},
		{ID: "g2", Symbol: "Si", Direction: gridShort, Status: "CLOSED",
			OpenedAt: "2026-09-21T10:00:00Z", ClosedAt: "2026-09-21T15:00:00Z",
			FinalPnl: -500, Fills: 2, RealizedGrid: -100, FeePerFill: 4,
			FillLog: []gridFill{{Kind: "LADDER"}}},
		{ID: "g3", Symbol: "RI", Direction: gridLong, Status: "OPEN", PositionID: "pos-3",
			OpenedAt: "2026-09-22T10:00:00Z", Fills: 1, RealizedGrid: -4, FeePerFill: 4,
			FillLog: []gridFill{{Kind: "LADDER"}}},
	}
	live := map[string]struct{ Pnl, Theta float64 }{"pos-3": {200, -50}}
	st := aggregateProtectGridStats(recs, live, nil)
	if st.Total != 3 || st.Open != 1 || st.Closed != 2 {
		t.Fatalf("counts wrong: %+v", st)
	}
	if st.Wins != 1 || st.WinRate != 50 {
		t.Fatalf("want 1 win of 2 closed = 50%%, got wins=%d rate=%v", st.Wins, st.WinRate)
	}
	if st.TotalPnl != 1500-500+200 {
		t.Fatalf("want total 1200, got %v", st.TotalPnl)
	}
	if st.Takes != 2 || st.AvgTake != 21 {
		t.Fatalf("want 2 takes avg 21, got %d / %v", st.Takes, st.AvgTake)
	}
	if st.Fills != 9 || st.Fees != 36 {
		t.Fatalf("want 9 fills / 36 fees, got %d / %v", st.Fills, st.Fees)
	}
	if st.ThetaDayOpen != -50 {
		t.Fatalf("want open theta -50, got %v", st.ThetaDayOpen)
	}
	if len(st.Rows) != 3 || !st.Rows[0].PnlKnown || st.Rows[0].Pnl != 1500 {
		t.Fatalf("rows wrong: %+v", st.Rows)
	}
}

func TestAggregateProtectGridStatsEmpty(t *testing.T) {
	st := aggregateProtectGridStats(nil, nil, nil)
	if st.Total != 0 || st.Rows == nil || st.WinRate != 0 {
		t.Fatalf("empty must be zero with non-nil rows: %+v", st)
	}
}

func TestMatchGridTradeHealsOldClose(t *testing.T) {
	opened := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	rec := protectGridRecord{ID: "old", Symbol: "Si", Direction: gridShort,
		Status: "CLOSED", OpenedAt: opened.Format(time.RFC3339)}
	trades := []quant.Trade{
		{ID: "trd-1", Strategy: "Protective Grid", Symbol: "Si",
			OpenedAt:   opened.Add(2 * time.Minute),
			EntryValue: 11390, ExitValue: 22083, RealizedPnL: 10693},
		{ID: "trd-2", Strategy: "Bull Put Spread", Symbol: "Si",
			OpenedAt: opened.Add(time.Minute), RealizedPnL: 999},
	}
	used := map[string]bool{}
	m := matchGridTrade(rec, trades, used)
	if m == nil || m.ID != "trd-1" {
		t.Fatalf("must match the grid trade, got %+v", m)
	}
	st := aggregateProtectGridStats([]protectGridRecord{rec}, nil, trades)
	if len(st.Rows) != 1 || !st.Rows[0].PnlKnown || st.Rows[0].Pnl != 10693 {
		t.Fatalf("healed row wrong: %+v", st.Rows)
	}
	if st.Rows[0].EntryValue != 11390 || st.Rows[0].ExitValue != 22083 {
		t.Fatalf("entry/exit must ride along: %+v", st.Rows[0])
	}
	if st.Wins != 1 || st.WinRate != 100 {
		t.Fatalf("healed win must count: wins=%d rate=%v", st.Wins, st.WinRate)
	}
}

func TestMatchGridTradeNoDoubleAssign(t *testing.T) {
	opened := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	mk := func(id string) protectGridRecord {
		return protectGridRecord{ID: id, Symbol: "Si", Status: "CLOSED",
			OpenedAt: opened.Format(time.RFC3339)}
	}
	trades := []quant.Trade{
		{ID: "trd-1", Strategy: "Protective Grid", Symbol: "Si",
			OpenedAt: opened.Add(time.Minute), RealizedPnL: 100},
	}
	used := map[string]bool{}
	if matchGridTrade(mk("a"), trades, used) == nil {
		t.Fatalf("first record must match")
	}
	used["trd-1"] = true
	if matchGridTrade(mk("b"), trades, used) != nil {
		t.Fatalf("one trade must not heal two records")
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
