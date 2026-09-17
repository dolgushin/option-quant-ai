package main

import (
	"testing"
	"time"
)

func TestDecideStraddleHedgeBands(t *testing.T) {
	// Delta band: fires at/above, quiet below; dust rounds to zero.
	fire, qty, reason := decideStraddleHedge(1.6, 86000, 86000, 0,
		straddleHedgeRules{Rule: hedgeDeltaBand, DeltaBand: 1.5})
	if !fire || qty != -2 || reason == "" {
		t.Fatalf("band must fire: %v %d %q", fire, qty, reason)
	}
	if fire, _, _ := decideStraddleHedge(1.0, 86000, 86000, 0,
		straddleHedgeRules{Rule: hedgeDeltaBand, DeltaBand: 1.5}); fire {
		t.Fatal("band must stay quiet below threshold")
	}
	if fire, qty, _ := decideStraddleHedge(0.4, 86000, 86000, 0,
		straddleHedgeRules{Rule: hedgeDeltaBand, DeltaBand: 0.1}); fire || qty != 0 {
		t.Fatalf("dust must round to no-hedge: %v %d", fire, qty)
	}
	// Time: fires after the interval, quiet before.
	fire, _, _ = decideStraddleHedge(0.8, 86000, 86000, 61,
		straddleHedgeRules{Rule: hedgeTime, IntervalMin: 60})
	if !fire {
		t.Fatal("time rule must fire after interval")
	}
	if fire, _, _ := decideStraddleHedge(2.5, 86000, 86000, 10,
		straddleHedgeRules{Rule: hedgeTime, IntervalMin: 60}); fire {
		t.Fatal("time rule must not fire early even on big delta")
	}
	// Hybrid: big move overrides the clock.
	fire, _, reason = decideStraddleHedge(3.2, 86000, 86000, 5,
		straddleHedgeRules{Rule: hedgeHybrid, DeltaBand: 1.5, BigMoveMult: 2, IntervalMin: 60})
	if !fire || reason == "" {
		t.Fatal("hybrid must fire on 2x band")
	}
	// Price band: 1% move fires, 0.2% rests; no mark forces first hedge.
	fire, _, _ = decideStraddleHedge(-1.2, 86860, 86000, 0,
		straddleHedgeRules{Rule: hedgePriceBand, PriceBandPct: 1.0})
	if !fire {
		t.Fatal("price band must fire on 1% move")
	}
	if fire, _, _ := decideStraddleHedge(-1.2, 86172, 86000, 0,
		straddleHedgeRules{Rule: hedgePriceBand, PriceBandPct: 1.0}); fire {
		t.Fatal("price band must rest inside the band")
	}
	fire, _, _ = decideStraddleHedge(-1.2, 86000, 0, 0,
		straddleHedgeRules{Rule: hedgePriceBand, PriceBandPct: 1.0})
	if !fire {
		t.Fatal("price band without a mark must hedge once")
	}
}

func TestBuildShortStraddle(t *testing.T) {
	pricer := func(call, put, fut string) (float64, float64, float64, error) {
		return 300, 280, 86000, nil
	}
	// Naked.
	p, err := buildShortStraddle("Si", "2026-12-17", 90, 86200, 86000, 2, false, "C", "P", "", pricer)
	if err != nil {
		t.Fatalf("naked: %v", err)
	}
	if len(p.Legs) != 2 {
		t.Fatalf("naked legs = %d, want 2", len(p.Legs))
	}
	// (300+280) × 1 × 2 = 1160 credit, stop 2320, margin 2320.
	if p.NetCredit != 1160 || p.StopLevel != 2320 || p.MarginEst != 2320 {
		t.Fatalf("naked econ = %v/%v/%v, want 1160/2320/2320", p.NetCredit, p.StopLevel, p.MarginEst)
	}
	// Covered adds the futures leg; economics unchanged (conservative).
	pc, err := buildShortStraddle("Si", "2026-12-17", 90, 86200, 86000, 2, true, "C", "P", "SiZ6", pricer)
	if err != nil {
		t.Fatalf("covered: %v", err)
	}
	if len(pc.Legs) != 3 || !pc.Legs[2].IsFuture || pc.Legs[2].Side != "BUY" {
		t.Fatalf("covered legs wrong: %+v", pc.Legs)
	}
	if pc.NetCredit != 1160 {
		t.Fatalf("covered credit = %v, want 1160", pc.NetCredit)
	}
	// Broken prices refuse.
	bad := func(call, put, fut string) (float64, float64, float64, error) { return 0, 280, 0, nil }
	if _, err := buildShortStraddle("Si", "2026-12-17", 90, 86200, 86000, 1, false, "C", "P", "", bad); err == nil {
		t.Fatal("zero call price must refuse")
	}
	if _, err := buildShortStraddle("Si", "2026-12-17", 90, 86200, 0, 1, false, "C", "P", "", pricer); err == nil {
		t.Fatal("zero strike must refuse")
	}
}

// TestStraddleStoreRoundtrip pins the JSON registry on a temp dir without
// touching the production store.
func TestStraddleStoreRoundtrip(t *testing.T) {
	straddleMu.Lock()
	prevFile, prevStore := straddleFile, straddleStore
	straddleStore = nil
	straddleMu.Unlock()
	defer func() {
		straddleMu.Lock()
		straddleFile, straddleStore = prevFile, prevStore
		straddleMu.Unlock()
	}()

	dir := t.TempDir()
	initStraddles(dir)
	if got := openStraddles(); len(got) != 0 {
		t.Fatalf("fresh store must be empty, got %d", len(got))
	}
	rec := straddleRecord{ID: "str-t1", Symbol: "Si", Status: "OPEN", Hedge: resolveHedgeRules(straddleHedgeRules{})}
	saveStraddleRecord(rec)
	got, found := straddleByID("str-t1")
	if !found || got.Symbol != "Si" {
		t.Fatalf("stored record not found: %+v %v", got, found)
	}
	if n := len(openStraddles()); n != 1 {
		t.Fatalf("open count = %d, want 1", n)
	}
	rec.Status = "CLOSED"
	saveStraddleRecord(rec)
	if n := len(openStraddles()); n != 0 {
		t.Fatalf("closed record still listed, count = %d", n)
	}
	// Reload from disk.
	initStraddles(dir)
	got, found = straddleByID("str-t1")
	if !found || got.Status != "CLOSED" {
		t.Fatalf("reload failed: %+v %v", got, found)
	}
}

// TestBuildExpiryOptions pins tenor markers and past-date filtering.
func TestBuildExpiryOptions(t *testing.T) {
	today := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	got := buildExpiryOptions([]string{
		"2026-09-09", "2026-09-09", "2026-09-03", "2026-09-24", "2026-12-17",
	}, today)
	if len(got) != 3 {
		t.Fatalf("got %d options, want 3 (dedup + drop past)", len(got))
	}
	// dteInDays truncates: Sep9→1d W, Sep24→16d M, Dec17→100d Q.
	want := []expiryOption{
		{Date: "2026-09-09", DTE: 1, Tenor: "W"},
		{Date: "2026-09-24", DTE: 16, Tenor: "M"},
		{Date: "2026-12-17", DTE: 100, Tenor: "Q"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("option %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBuildStrikeOptions pins the ATM divider placement.
func TestBuildStrikeOptions(t *testing.T) {
	rows := buildStrikeOptions([]float64{87000, 85000, 86000}, 86000)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 3 strikes + divider", len(rows))
	}
	if !rows[1].Divider {
		t.Fatalf("divider must sit right above ATM: %+v", rows)
	}
	if rows[2].Strike != 86000 || !rows[2].ATM {
		t.Fatalf("ATM row wrong: %+v", rows)
	}
	if rows[0].Strike != 85000 || rows[3].Strike != 87000 {
		t.Fatalf("sorting wrong: %+v", rows)
	}
	// No ATM (no spot): plain sorted list, no divider.
	plain := buildStrikeOptions([]float64{86000, 85000}, 0)
	if len(plain) != 2 || plain[0].Divider || plain[1].Divider {
		t.Fatalf("no-ATM must have no divider: %+v", plain)
	}
}

func TestStraddleStops(t *testing.T) {
	rec := &straddleRecord{StopLevel: 2320, TimeStopDTE: 14}
	if !straddleShouldStop(rec, -2320) {
		t.Fatal("stop must fire at exactly 2x premium")
	}
	if straddleShouldStop(rec, -2319) {
		t.Fatal("stop must not fire above the level")
	}
	if !straddleShouldTimeStop(rec, 14) || !straddleShouldTimeStop(rec, 3) {
		t.Fatal("time-stop must fire at/below 14 DTE")
	}
	if straddleShouldTimeStop(rec, 15) {
		t.Fatal("time-stop must not fire above 14 DTE")
	}
	def := &straddleRecord{}
	if !straddleShouldTimeStop(def, 14) {
		t.Fatal("default time-stop is 14 DTE")
	}
	if straddleShouldStop(nil, -999999) {
		t.Fatal("nil record must not stop")
	}
	if minutesSince("", time.Now()) < 1e8 {
		t.Fatal("empty mark must read as ancient")
	}
}
