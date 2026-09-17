package main

import (
	"testing"
	"time"

	"option-quant-ai/alor"
)

func TestDecideStraddleHedgeBands(t *testing.T) {
	// Delta band: fires at/above, quiet below; dust rounds to zero.
	// (target 0 = naked straddle.)
	fire, qty, reason := decideStraddleHedge(1.6, 0, 86000, 86000, 0,
		straddleHedgeRules{Rule: hedgeDeltaBand, DeltaBand: 1.5})
	if !fire || qty != -2 || reason == "" {
		t.Fatalf("band must fire: %v %d %q", fire, qty, reason)
	}
	if fire, _, _ := decideStraddleHedge(1.0, 0, 86000, 86000, 0,
		straddleHedgeRules{Rule: hedgeDeltaBand, DeltaBand: 1.5}); fire {
		t.Fatal("band must stay quiet below threshold")
	}
	if fire, qty, _ := decideStraddleHedge(0.4, 0, 86000, 86000, 0,
		straddleHedgeRules{Rule: hedgeDeltaBand, DeltaBand: 0.1}); fire || qty != 0 {
		t.Fatalf("dust must round to no-hedge: %v %d", fire, qty)
	}
	// Time: fires after the interval, quiet before.
	fire, _, _ = decideStraddleHedge(0.8, 0, 86000, 86000, 61,
		straddleHedgeRules{Rule: hedgeTime, IntervalMin: 60})
	if !fire {
		t.Fatal("time rule must fire after interval")
	}
	if fire, _, _ := decideStraddleHedge(2.5, 0, 86000, 86000, 10,
		straddleHedgeRules{Rule: hedgeTime, IntervalMin: 60}); fire {
		t.Fatal("time rule must not fire early even on big delta")
	}
	// Hybrid: big move overrides the clock.
	fire, _, reason = decideStraddleHedge(3.2, 0, 86000, 86000, 5,
		straddleHedgeRules{Rule: hedgeHybrid, DeltaBand: 1.5, BigMoveMult: 2, IntervalMin: 60})
	if !fire || reason == "" {
		t.Fatal("hybrid must fire on 2x band")
	}
	// Price band: 1% move fires, 0.2% rests; no mark forces first hedge.
	fire, _, _ = decideStraddleHedge(-1.2, 0, 86860, 86000, 0,
		straddleHedgeRules{Rule: hedgePriceBand, PriceBandPct: 1.0})
	if !fire {
		t.Fatal("price band must fire on 1% move")
	}
	if fire, _, _ := decideStraddleHedge(-1.2, 0, 86172, 86000, 0,
		straddleHedgeRules{Rule: hedgePriceBand, PriceBandPct: 1.0}); fire {
		t.Fatal("price band must rest inside the band")
	}
	fire, _, _ = decideStraddleHedge(-1.2, 0, 86000, 0, 0,
		straddleHedgeRules{Rule: hedgePriceBand, PriceBandPct: 1.0})
	if !fire {
		t.Fatal("price band without a mark must hedge once")
	}
}

// TestDecideStraddleHedgeCovered pins the covered-straddle target: the +Qty
// futures cover is maintained, not flattened — a covered position sitting
// exactly on its cover must rest, drift hedges back toward it.
func TestDecideStraddleHedgeCovered(t *testing.T) {
	band := straddleHedgeRules{Rule: hedgeDeltaBand, DeltaBand: 1.5}
	// Sitting on the cover: no hedge (the old target-0 engine flattened here).
	if fire, qty, _ := decideStraddleHedge(1.0, 1.0, 86000, 86000, 0, band); fire || qty != 0 {
		t.Fatalf("on-cover must rest: %v %d", fire, qty)
	}
	// Drifted up to +3 with +1 cover: sell 2 back toward the cover.
	fire, qty, _ := decideStraddleHedge(3.0, 1.0, 87000, 86000, 0, band)
	if !fire || qty != -2 {
		t.Fatalf("drift must hedge toward cover: %v %d", fire, qty)
	}
	// Drifted down to -1 with +1 cover: buy 2 to restore the cover.
	fire, qty, _ = decideStraddleHedge(-1.0, 1.0, 85000, 86000, 0, band)
	if !fire || qty != 2 {
		t.Fatalf("under-cover must buy back: %v %d", fire, qty)
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

// TestSellPriceFromBook pins executable SELL pricing: mid when two-sided,
// best bid when the book is one-sided (evening), refuse on empty bids.
func TestSellPriceFromBook(t *testing.T) {
	mk := func(bids, asks [][2]float64) alor.AlorOrderbookResponse {
		ob := alor.AlorOrderbookResponse{}
		for _, b := range bids {
			ob.Bids = append(ob.Bids, alor.OrderbookEntry{Price: b[0], Volume: int(b[1])})
		}
		for _, a := range asks {
			ob.Asks = append(ob.Asks, alor.OrderbookEntry{Price: a[0], Volume: int(a[1])})
		}
		return ob
	}
	px, ok := sellPriceFromBook(mk([][2]float64{{725, 6}}, [][2]float64{{1100, 1}}))
	if !ok || px != 912.5 {
		t.Fatalf("two-sided = mid 912.5, got %v %v", px, ok)
	}
	px, ok = sellPriceFromBook(mk([][2]float64{{725, 6}}, nil))
	if !ok || px != 725 {
		t.Fatalf("bid-only = best bid 725, got %v %v", px, ok)
	}
	// Crossed book still sells at the bid.
	px, ok = sellPriceFromBook(mk([][2]float64{{800, 1}}, [][2]float64{{700, 1}}))
	if !ok || px != 800 {
		t.Fatalf("crossed = bid 800, got %v %v", px, ok)
	}
	if _, ok := sellPriceFromBook(mk(nil, [][2]float64{{1100, 1}})); ok {
		t.Fatal("no bids must refuse")
	}
	if _, ok := sellPriceFromBook(alor.AlorOrderbookResponse{}); ok {
		t.Fatal("empty book must refuse")
	}
}

// TestParseStrikesFromSymbols pins Alor-search strike parsing: digits right
// after the root pass, everything else (futures, stocks, garbage) drops out.
func TestParseStrikesFromSymbols(t *testing.T) {
	syms := []string{"Si86000BU6", "Si86500BX6", "Si86000BU6", "SiU6", "SIBN", "SiABC", "RI90000ZZ9", ""}
	got := parseStrikesFromSymbols(syms, "Si")
	if len(got) != 2 || got[0] != 86000 || got[1] != 86500 {
		t.Fatalf("Si strikes = %v, want [86000 86500]", got)
	}
	gotRI := parseStrikesFromSymbols(syms, "RI")
	if len(gotRI) != 1 || gotRI[0] != 90000 {
		t.Fatalf("RI strikes = %v, want [90000]", gotRI)
	}
	if got := parseStrikesFromSymbols(nil, "Si"); len(got) != 0 {
		t.Fatalf("nil must give empty, got %v", got)
	}
}

// TestParseFuturesCode pins front-futures month math (Sep 2026 context).
func TestParseFuturesCode(t *testing.T) {
	root, y, m, ok := parseFuturesCode("SiU6")
	if !ok || root != "Si" || m != time.September {
		t.Fatalf("SiU6 = %q %d %v %v", root, y, m, ok)
	}
	if _, _, _, ok := parseFuturesCode("Si86000BU6"); ok {
		t.Fatal("option secid must not parse as futures")
	}
	if _, _, _, ok := parseFuturesCode("SIBN"); ok {
		t.Fatal("stock must not parse as futures")
	}
	if _, _, _, ok := parseFuturesCode("Si"); ok {
		t.Fatal("bare root must not parse")
	}
}

// TestResolveFuturesAlor picks the nearest contract at/after now.
func TestResolveFuturesAlor(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	syms := []string{"SiU6", "SiZ6", "SiH7", "Si86000BU6", "SiF6", "SIBN", "RIU6"}
	if got := resolveFuturesAlor(syms, "Si", now); got != "SiU6" {
		t.Fatalf("front = %q, want SiU6", got)
	}
	// After September expiry the front rolls to December.
	late := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if got := resolveFuturesAlor(syms, "Si", late); got != "SiU6" {
		t.Fatalf("month-granular front = %q, want SiU6 (month not over)", got)
	}
	if got := resolveFuturesAlor(syms, "RI", now); got != "RIU6" {
		t.Fatalf("RI front = %q, want RIU6", got)
	}
	if got := resolveFuturesAlor(nil, "Si", now); got != "" {
		t.Fatalf("empty search must give empty, got %q", got)
	}
}

// TestParseAlorOptionInfo checks tolerant typing without depending on the
// exact Alor schema.
func TestParseAlorOptionInfo(t *testing.T) {
	_, _, kind := parseAlorOptionInfo(map[string]interface{}{"optionType": "Call"})
	if kind != "call" {
		t.Fatalf("optionType Call = %q, want call", kind)
	}
	_, _, kind = parseAlorOptionInfo(map[string]interface{}{"description": "Si 86000 Put"})
	if kind != "put" {
		t.Fatalf("description Put = %q, want put", kind)
	}
	s, _, kind := parseAlorOptionInfo(map[string]interface{}{"strike": 86000.0, "shortname": "Si86000BU6"})
	if s != 86000 || kind != "" {
		t.Fatalf("BU6 shortname must give strike only, got %v %q", s, kind)
	}
	if _, _, kind := parseAlorOptionInfo(map[string]interface{}{}); kind != "" {
		t.Fatalf("empty map must give empty kind, got %q", kind)
	}
}

// TestThetaAccrualRisesForShort straddle time decay must accrue upward
// (short premium earns) starting from exactly zero.
func TestThetaAccrualRisesForShort(t *testing.T) {
	legs := []analyticsLeg{
		{Side: "SELL", Kind: "OPTION", Strike: 86000, IsCall: true, Quantity: 1, Iv: 20},
		{Side: "SELL", Kind: "OPTION", Strike: 86000, IsCall: false, Quantity: 1, Iv: 20},
	}
	xs, cumul := thetaAccrualCurve(legs, 86000, 30, 1, 14)
	if len(xs) != 15 || len(cumul) != 15 {
		t.Fatalf("want 15 points, got %d/%d", len(xs), len(cumul))
	}
	if cumul[0] != 0 {
		t.Fatalf("day 0 must be 0, got %v", cumul[0])
	}
	for i := 1; i < len(cumul); i++ {
		if cumul[i] < cumul[i-1] {
			t.Fatalf("short accrual must not fall: day %d %v < %v", i, cumul[i], cumul[i-1])
		}
	}
	if cumul[14] <= 0 {
		t.Fatalf("14 days must accrue positive, got %v", cumul[14])
	}
}

// TestHedgeSpotForecast pins crossing search on a V-shaped delta curve.
func TestHedgeSpotForecast(t *testing.T) {
	spots := []float64{84000, 85000, 86000, 87000, 88000}
	deltas := []float64{-2.0, -0.5, 0.1, 0.8, 2.2}
	lo, hi := hedgeSpotForecast(spots, deltas, 2, 1.0)
	if lo != 84000 || hi != 88000 {
		t.Fatalf("crossings = %v/%v, want 84000/88000", lo, hi)
	}
	lo, hi = hedgeSpotForecast(spots, deltas, 2, 5.0)
	if lo != 0 || hi != 0 {
		t.Fatalf("untouched band must give zeros, got %v/%v", lo, hi)
	}
	lo, hi = hedgeSpotForecast(spots, deltas, -1, 1.0)
	if lo != 0 || hi != 0 {
		t.Fatalf("bad index must give zeros, got %v/%v", lo, hi)
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
