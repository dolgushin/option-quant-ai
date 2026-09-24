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
	// LONG works strength only: nothing at/under the anchor, first unit one
	// step above it — never an instant entry at the anchor itself.
	if got := gridLadderTarget(gridLong, 86000, 86000, 25, 5); got != 0 {
		t.Fatalf("at anchor long must hold 0, got %d", got)
	}
	if got := gridLadderTarget(gridLong, 86000, 85940, 25, 5); got != 0 {
		t.Fatalf("below anchor long must hold 0, got %d", got)
	}
	if got := gridLadderTarget(gridLong, 86000, 86025, 25, 5); got != 1 {
		t.Fatalf("one step above = 1 unit, got %d", got)
	}
	if got := gridLadderTarget(gridLong, 86000, 86100, 25, 5); got != 4 {
		t.Fatalf("100pts above = 4 units, got %d", got)
	}
	if got := gridLadderTarget(gridLong, 86000, 87000, 25, 5); got != 5 {
		t.Fatalf("must clamp at max, got %d", got)
	}
	// SHORT mirror: sells below the anchor only.
	if got := gridLadderTarget(gridShort, 86000, 86000, 25, 4); got != 0 {
		t.Fatalf("at anchor short must hold 0, got %d", got)
	}
	if got := gridLadderTarget(gridShort, 86000, 85940, 25, 4); got != 2 {
		t.Fatalf("60pts under anchor = 2 shorts, got %d", got)
	}
	if got := gridLadderTarget("SIDEWAYS", 86000, 85900, 25, 4); got != 0 {
		t.Fatalf("unknown direction → 0, got %d", got)
	}
}

func TestTrailGridAnchor(t *testing.T) {
	// Flat grids never slide — fixed rungs re-arm themselves.
	if a := trailGridAnchor(gridLong, 86000, 86100, 25, 5, 0); a != 86000 {
		t.Fatalf("virgin flat must hold entry, got %v", a)
	}
	if a := trailGridAnchor(gridLong, 86000, 85000, 25, 5, 0); a != 86000 {
		t.Fatalf("traded flat must hold (downside is the put's job), got %v", a)
	}
	// Fully loaded grid outrun upward shifts the ladder along (risk capped).
	if a := trailGridAnchor(gridLong, 86000, 87000, 25, 5, 5); a != 86875 {
		t.Fatalf("full grid must recenter behind price, got %v", a)
	}
	// Fully loaded grid inside coverage holds the anchor.
	if a := trailGridAnchor(gridLong, 86000, 86100, 25, 5, 5); a != 86000 {
		t.Fatalf("covered full grid must hold, got %v", a)
	}
	// Partial inventory never trails.
	if a := trailGridAnchor(gridLong, 86000, 87000, 25, 5, 2); a != 86000 {
		t.Fatalf("working grid must hold, got %v", a)
	}
	// SHORT mirror: full grid outrun downward recenters down.
	if a := trailGridAnchor(gridShort, 86000, 85900, 25, 5, 0); a != 86000 {
		t.Fatalf("short flat must hold, got %v", a)
	}
	if a := trailGridAnchor(gridShort, 86000, 85000, 25, 5, 5); a != 85125 {
		t.Fatalf("short full must recenter, got %v", a)
	}
}

func TestSimulateGridPyramidsUp(t *testing.T) {
	// Steady rally with a single-unit ladder: every rung banks its circle.
	path := []float64{86000, 86025, 86050, 86075, 86100}
	real, _, fills, inv, _ := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 1)
	if real != 75 || fills != 7 || inv != 1 {
		t.Fatalf("want rung-by-rung banking (75, 7 fills, 1 held), got real=%v fills=%d inv=%d", real, fills, inv)
	}
}

func TestSimulateGridIgnoresFall(t *testing.T) {
	// Fall first: the LONG ladder must NOT average down — puts own the
	// downside. The return rally then loads the ladder and banks one circle.
	path := []float64{86000, 85975, 85950, 85950, 85975, 86000, 86025, 86050}
	real, _, fills, inv, _ := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 2)
	if real != 25 || inv != 2 || fills != 4 {
		t.Fatalf("want fall ignored, rally loaded (+25, 2 held, 4 fills), got real=%v inv=%d fills=%d", real, inv, fills)
	}
}

func TestBuildPgridChart(t *testing.T) {
	// LONG + PUT 85500 ×10, anchor 85875, step 50, spot 86803.
	ch := buildPgridChart(gridLong, 85500, 1165, false, 10, 1, 85875, 50, 86803, 85875, 10, nil, 0.30, 20.0/365.0)
	if len(ch.Rungs) != 10 || ch.Rungs[0] != 85925 || ch.Rungs[9] != 86375 {
		t.Fatalf("long rungs must step up from anchor, got %v", ch.Rungs)
	}
	if len(ch.Spots) != 61 || len(ch.WingExpiry) != 61 {
		t.Fatalf("want 61 curve points, got %d/%d", len(ch.Spots), len(ch.WingExpiry))
	}
	// Far above the strike the long put expires worthless: -premium.
	last := ch.WingExpiry[len(ch.WingExpiry)-1]
	if last != -11650 {
		t.Fatalf("OTM put expiry must be -11650, got %v", last)
	}
	// Near the strike the loss is ~the full premium (grid resolution ±1 step).
	found := false
	for i, s := range ch.Spots {
		if math.Abs(s-85500) < 60 {
			if ch.WingExpiry[i] < -11650 || ch.WingExpiry[i] > -11000 {
				t.Fatalf("near-strike put must be ~-premium, got %v", ch.WingExpiry[i])
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("curve must cover the strike")
	}
	for _, m := range []string{"spot", "entry", "anchor", "strike"} {
		if _, ok := ch.Markers[m]; !ok {
			t.Fatalf("missing marker %s", m)
		}
	}
	// Current wing P&L rides above the expiry payoff (long time value).
	if len(ch.WingNow) != 61 {
		t.Fatalf("want 61 now-curve points, got %d", len(ch.WingNow))
	}
	for i := range ch.WingNow {
		if ch.WingNow[i] < ch.WingExpiry[i] {
			t.Fatalf("long wing now must beat expiry payoff at %v: %v < %v", ch.Spots[i], ch.WingNow[i], ch.WingExpiry[i])
		}
	}
	// No IV → no now-curve (never a half-built one).
	chNoIV := buildPgridChart(gridLong, 85500, 1165, false, 10, 1, 85875, 50, 86803, 85875, 10, nil, 0, 0)
	if chNoIV.WingNow != nil {
		t.Fatalf("without IV the now-curve must be absent")
	}
	// SHORT mirror: rungs below the anchor; deep ITM call pays intrinsic.
	chS := buildPgridChart(gridShort, 86000, 1200, true, 1, 1, 85900, 25, 89000, 85900, 5,
		[]pgridLotMark{{Entry: 88900, TP: 88875, Qty: 2}}, 0.30, 20.0/365.0)
	if len(chS.Rungs) != 5 || chS.Rungs[0] != 85875 {
		t.Fatalf("short rungs must step down, got %v", chS.Rungs)
	}
	if len(chS.Lots) != 1 || chS.Lots[0].TP != 88875 {
		t.Fatalf("lots must ride along: %+v", chS.Lots)
	}
	best := chS.WingExpiry[0]
	for _, v := range chS.WingExpiry {
		if v > best {
			best = v
		}
	}
	if best <= 0 {
		t.Fatalf("deep ITM call curve must go positive, best=%v", best)
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

func TestSimulateGridFlatIdle(t *testing.T) {
	path := synthPath("flat", 86000, 40, 0)
	real, _, fills, inv, _ := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 5)
	// Flat at entry: no rung touched, grid waits (theta bleeds on the wing).
	if inv != 0 || fills != 0 || real != 0 {
		t.Fatalf("flat must stay out: real=%v fills=%d inv=%d", real, fills, inv)
	}
}

func TestSimulateGridTrendAgainstHoldsZero(t *testing.T) {
	path := synthPath("trend_down", 86000, 160, 2000)
	real, unreal, fills, inv, _ := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 5)
	if inv != 0 || fills != 0 || real != 0 || unreal != 0 {
		t.Fatalf("downtrend must not load a LONG ladder: real=%v unreal=%v fills=%d inv=%d", real, unreal, fills, inv)
	}
}

func TestSimulateGridTrendWithUs(t *testing.T) {
	// LONG grid into an up-trend: rungs load on strength and bank circles.
	path := synthPath("trend_up", 86000, 160, 500)
	real, _, fills, inv, _ := simulateGrid(gridLong, path, 86000, 25, 25, 1, 0, 1, 5)
	if real <= 0 || fills == 0 || inv <= 0 {
		t.Fatalf("trend with us must earn: real=%v fills=%d inv=%d", real, fills, inv)
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
