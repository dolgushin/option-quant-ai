package main

// Protective-grid module (third trading module): a long protective option
// (PUT for a long futures grid, CALL for a short grid) + a one-sided futures
// scalping grid that harvests intraday oscillation to pay for theta.
//
// Idea (user strategy): after Si exhausts its daily range downwards, the odds
// of a bounce rise — buy 1-step-OTM puts (cheap tail cover) and run a LONG
// only futures grid (buy dips every `step` points, take each unit at +`tp`).
// If Si ran up instead, mirror: buy 1-step-OTM calls + SHORT grid.
//
// Portfolio character: long downside/upside tail + long chop (grid + long
// option are both long-gamma style). It earns when the market oscillates
// around/after exhaustion and bleeds theta when it sits still or trends
// through the protection. See KNOWLEDGE.md §9 for the regime table.
//
// Conventions (same as straddles): paper-first, no estimate prices, weekend/
// night halt, futures legs marked at their own contract, opposite futures
// legs net FIFO via quant.NetFuturesLegs with realized P&L preserved and
// journaled once via quant.SettleTrade. Core math is pure and hermetic.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"option-quant-ai/quant"
)

// Grid directions.
const (
	gridLong  = "LONG"  // buy dips, take profit up; protection = PUT
	gridShort = "SHORT" // sell rallies, take profit down; protection = CALL
)

// Signal tiers for range-exhaustion entries (share the daily-range tracker).
const (
	gridTierHalf      = "HALF"      // ~50% of the daily move — early/scout entry
	gridTierReady     = "READY"     // ~65-85% — standard entry
	gridTierExhausted = "EXHAUSTED" // ~90%+ — late but highest bounce odds
)

// gridSignal is a pure range-exhaustion read: which protective grid (if any)
// the tape invites right now.
type gridSignal struct {
	Direction string   `json:"direction"` // LONG | SHORT | ""
	Tier      string   `json:"tier"`      // HALF | READY | EXHAUSTED | ""
	RangePct  float64  `json:"range_pct_atr"`
	DriftPct  float64  `json:"drift_pct_atr"`
	Ready     bool     `json:"ready"`
	Reasons   []string `json:"reasons"`
}

// protectGridSignal maps today's (open/high/low/last) + ATR(14) to a grid
// direction + tier. Down day → LONG grid + PUT (bounce bet with a floor);
// up day → SHORT grid + CALL. Pure — unit-tested.
//
// Thresholds are on two gauges: rangePct = dayRange/ATR (how much of a
// normal day already travelled) and driftPct = |last-open|/ATR (how far the
// close sits from the open). Either gauge can trigger — a tight day with a
// late impulse still counts via drift, a wide chop day via range.
func protectGridSignal(open, high, low, last, atr float64) gridSignal {
	sig := gridSignal{}
	if atr <= 0 || high < low || last <= 0 || open <= 0 {
		sig.Reasons = []string{"нет ATR или битые котировки дня"}
		return sig
	}
	dayRange := high - low
	change := last - open
	sig.RangePct = math.Round(dayRange / atr * 1000 / 10)
	sig.DriftPct = math.Round(math.Abs(change) / atr * 1000 / 10)
	if change == 0 {
		sig.Reasons = []string{"день без направления — сетке нечего отрабатывать"}
		return sig
	}
	if change < 0 {
		sig.Direction = gridLong
	} else {
		sig.Direction = gridShort
	}
	cover := "PUT"
	if sig.Direction == gridShort {
		cover = "CALL"
	}
	gauge := math.Max(sig.RangePct, sig.DriftPct)
	switch {
	case gauge >= 90:
		sig.Tier, sig.Ready = gridTierExhausted, true
		sig.Reasons = []string{
			fmt.Sprintf("движение исчерпано (%.0f%% ATR) — шансы отката максимальны, защита %s", gauge, cover),
		}
	case gauge >= 65:
		sig.Tier, sig.Ready = gridTierReady, true
		sig.Reasons = []string{
			fmt.Sprintf("пройдено %.0f%% дневной нормы — стандартный вход, защита %s", gauge, cover),
		}
	case gauge >= 45:
		sig.Tier, sig.Ready = gridTierHalf, false
		sig.Reasons = []string{
			fmt.Sprintf("пройдена половина дневного хода (%.0f%% ATR) — ранний/разведочный вход половинным размером", gauge),
		}
	default:
		sig.Reasons = []string{
			fmt.Sprintf("пройдено лишь %.0f%% дневной нормы — ждать ≥45%% (половина) или ≥65%% (стандарт)", gauge),
		}
		sig.Direction, sig.Tier = "", ""
	}
	return sig
}

// gridBreakevenRoundTripsPerDay answers "how many grid round-trips a day pay
// for theta": ceil(thetaDay / (tp*mult - 2*fee)). One round trip earns the
// per-unit TAKE PROFIT (not the ladder step) and pays the entry AND the exit
// fee (the simulator charges both). A take profit below two fees means every
// circle loses money — Inf. Pure — unit-tested.
func gridBreakevenRoundTripsPerDay(thetaDay, tpPts, mult, feePerFill float64) float64 {
	edge := tpPts*mult - 2*feePerFill
	if edge <= 0 || thetaDay <= 0 {
		return math.Inf(1)
	}
	return math.Ceil(thetaDay / edge)
}

// finiteOrNil maps an unpayable breakeven (+Inf) to JSON null:
// encoding/json cannot marshal Inf and fails the whole response with an
// empty body. Pure — unit-tested.
func finiteOrNil(v float64) *float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return nil
	}
	return &v
}

// gridLadderTarget is the inventory target of a one-sided grid relative to
// its anchor: how many futures the ladder wants at this spot. The ladder
// works WITH the thesis, never against it: LONG buys strength — rungs sit
// ABOVE the anchor (first buy only at anchor+step, never at the anchor
// itself), the downside is the put's job; SHORT mirrors below the anchor
// with the call covering the upside. Clamped to [0, maxInv]. Pure —
// unit-tested.
func gridLadderTarget(direction string, anchor, spot, step float64, maxInv int) int {
	if step <= 0 || maxInv <= 0 || anchor <= 0 || spot <= 0 {
		return 0
	}
	var depth float64
	if direction == gridLong {
		depth = (spot - anchor) / step
	} else if direction == gridShort {
		depth = (anchor - spot) / step
	} else {
		return 0
	}
	target := int(math.Floor(depth + 1e-9))
	if target < 0 {
		target = 0
	}
	if target > maxInv {
		target = maxInv
	}
	return target
}

// trailGridAnchor shifts a fully loaded ladder behind a runaway price: LONG
// rungs sit above the anchor, so a rally past the far rung (spot above
// anchor+span with max inventory) moves the anchor to spot-span — the same
// zone keeps earning rung by rung instead of idling. No new risk is added
// (inventory is already capped). Anything else holds the anchor: rungs are
// fixed levels, so a pullback-then-rise re-arms the very same rung without
// any sliding, and a virgin grid never drifts before its first unit. Pure —
// unit-tested.
func trailGridAnchor(direction string, anchor, spot, step float64, maxInv, inventory int) float64 {
	if step <= 0 || maxInv <= 0 || anchor <= 0 || spot <= 0 {
		return anchor
	}
	span := float64(maxInv) * step
	switch direction {
	case gridLong:
		if inventory >= maxInv && spot > anchor+span {
			return spot - span
		}
		return anchor
	case gridShort:
		if inventory >= maxInv && spot < anchor-span {
			return spot + span
		}
		return anchor
	default:
		return anchor
	}
}

// gridOpenInventory counts the net one-sided futures inventory on a position
// (BUY minus SELL for LONG grids, mirrored for SHORT). Pure.
func gridOpenInventory(legs []quant.PositionLeg, direction string) int {
	net := 0
	for _, l := range legs {
		if l.Kind != "FUTURES" || l.Quantity <= 0 {
			continue
		}
		if direction == gridLong {
			if l.Side == "BUY" {
				net += l.Quantity
			} else {
				net -= l.Quantity
			}
		} else {
			if l.Side == "SELL" {
				net += l.Quantity
			} else {
				net -= l.Quantity
			}
		}
	}
	if net < 0 {
		net = 0
	}
	return net
}

// gridTakeProfitLegs finds open ladder legs whose per-unit take profit is
// touched at spot: BUY legs with entry+tp <= spot (LONG), SELL legs with
// entry-tp >= spot (SHORT). Returns the total closable qty. Pure.
func gridTakeProfitLegs(legs []quant.PositionLeg, direction string, spot, tpPts float64) int {
	if tpPts <= 0 || spot <= 0 {
		return 0
	}
	qty := 0
	for _, l := range legs {
		if l.Kind != "FUTURES" || l.Quantity <= 0 || l.EntryPrice <= 0 {
			continue
		}
		if direction == gridLong && l.Side == "BUY" && spot >= l.EntryPrice+tpPts {
			qty += l.Quantity
		}
		if direction == gridShort && l.Side == "SELL" && spot <= l.EntryPrice-tpPts {
			qty += l.Quantity
		}
	}
	return qty
}

// gridLot is one unit of simulated inventory for the offline simulator.
type gridLot struct {
	entry float64
	qty   int
}

// simulateGrid runs the trailing ladder + per-unit-TP policy over a price
// path and returns realized P&L (rubles), closing inventory + its unrealized
// value, fills and the max adverse excursion in points. Simulator only — the
// live manager replays the same policy one pass at a time. Pure —
// unit-tested.
func simulateGrid(direction string, prices []float64, entry, step, tpPts, mult, fee float64, qtyPerLevel, maxInv int) (realized, unrealized float64, fills, inventory, maxAdverse int) {
	if len(prices) == 0 || step <= 0 || mult <= 0 || qtyPerLevel <= 0 || maxInv <= 0 {
		return 0, 0, 0, 0, 0
	}
	lots := []gridLot{}
	anchor := entry
	adverse := 0.0
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	for _, px := range prices {
		if px <= 0 {
			continue
		}
		// 1) Take profits first (each unit has its own target).
		kept := lots[:0]
		for _, lt := range lots {
			hit := direction == gridLong && px >= lt.entry+tpPts
			if direction == gridShort && px <= lt.entry-tpPts {
				hit = true
			}
			if hit {
				var pnl float64
				if direction == gridLong {
					pnl = (px - lt.entry) * mult * float64(lt.qty)
				} else {
					pnl = (lt.entry - px) * mult * float64(lt.qty)
				}
				realized += pnl - fee*float64(lt.qty)
				fills++
				inventory -= lt.qty
			} else {
				kept = append(kept, lt)
			}
		}
		lots = kept
		// 2) Trail the anchor, then top up toward the target from it.
		anchor = trailGridAnchor(direction, anchor, px, step, maxInv, inventory)
		target := gridLadderTarget(direction, anchor, px, step, maxInv)
		for inventory < target {
			lots = append(lots, gridLot{entry: px, qty: qtyPerLevel})
			inventory += qtyPerLevel
			realized -= fee * float64(qtyPerLevel) // entry fee
			fills++
			if len(lots) > 4*maxInv+10 {
				break
			}
		}
		// 3) Track the worst open-water mark against the entry side.
		open := 0.0
		for _, lt := range lots {
			if direction == gridLong {
				open += (px - lt.entry) * mult * float64(lt.qty)
			} else {
				open += (lt.entry - px) * mult * float64(lt.qty)
			}
		}
		if open < adverse {
			adverse = open
		}
	}
	last := prices[len(prices)-1]
	for _, lt := range lots {
		if direction == gridLong {
			unrealized += (last - lt.entry) * mult * float64(lt.qty)
		} else {
			unrealized += (lt.entry - last) * mult * float64(lt.qty)
		}
	}
	realized = round(realized)
	unrealized = round(unrealized)
	maxAdverse = int(math.Round(-adverse))
	if maxAdverse < 0 {
		maxAdverse = 0
	}
	return realized, unrealized, fills, inventory, maxAdverse
}

// synthPath builds deterministic scenario paths around `start`: flat, chop
// (sine of amplitude `amp`), trend up/down. Pure — for plan scenarios.
func synthPath(kind string, start float64, n int, amp float64) []float64 {
	if n <= 0 {
		n = 120
	}
	out := make([]float64, 0, n)
	switch kind {
	case "flat":
		for i := 0; i < n; i++ {
			out = append(out, start)
		}
	case "chop":
		for i := 0; i < n; i++ {
			out = append(out, start+amp*math.Sin(float64(i)*0.6))
		}
	case "trend_up":
		for i := 0; i < n; i++ {
			out = append(out, start+amp*float64(i)/float64(n))
		}
	case "trend_down":
		for i := 0; i < n; i++ {
			out = append(out, start-amp*float64(i)/float64(n))
		}
	default:
		for i := 0; i < n; i++ {
			out = append(out, start)
		}
	}
	return out
}

// ---- Records & store (JSON file, mirrors straddles) ----

type gridFill struct {
	At        string  `json:"at"`
	Side      string  `json:"side"`
	Qty       int     `json:"qty"`
	Price     float64 `json:"price"`
	Kind      string  `json:"kind"`                 // "LADDER" | "TAKE" | "OPEN" | "CLOSE"
	ClosedPnl float64 `json:"closed_pnl,omitempty"` // netted futures P&L closed by this fill (TAKE), rubles ex-fees
	Note      string  `json:"note,omitempty"`
}

// protectGridRecord is a live paper protective grid.
type protectGridRecord struct {
	ID            string        `json:"id"`
	PositionID    string        `json:"position_id"`
	Symbol        string        `json:"symbol"`
	Direction     string        `json:"direction"` // LONG | SHORT
	Expiry        string        `json:"expiry"`
	EntrySpot     float64       `json:"entry_spot"`
	ProtectSecID  string        `json:"protect_secid"`
	ProtectStrike float64       `json:"protect_strike"`
	ProtectIsCall bool          `json:"protect_is_call"`
	ProtectQty    int           `json:"protect_qty"`
	ProtectEntry  float64       `json:"protect_entry"`
	Anchor        float64       `json:"anchor"` // trailing ladder anchor: entry at open, then follows price
	GridStep      float64       `json:"grid_step"`
	TakeProfit    float64       `json:"take_profit"`
	QtyPerLevel   int           `json:"qty_per_level"`
	MaxInventory  int           `json:"max_inventory"`
	FeePerFill    float64       `json:"fee_per_fill"`
	FuturesSecID  string        `json:"futures_secid"`
	MaxLossRub    float64       `json:"max_loss_rub"`
	ProfitTarget  float64       `json:"profit_target_rub"`
	TimeStopDTE   int           `json:"time_stop_dte"`
	RangePctAt    float64       `json:"range_pct_at_entry"`
	Status        string        `json:"status"` // OPEN / CLOSED
	OpenedAt      string        `json:"opened_at"`
	ClosedAt      string        `json:"closed_at,omitempty"`
	CloseReason   string        `json:"close_reason,omitempty"`
	FinalPnl      float64       `json:"final_pnl,omitempty"` // closed-grid total (live P&L + netted realized), rubles
	EntryValue    float64       `json:"entry_value,omitempty"`
	ExitLegs      []gridExitLeg `json:"exit_legs,omitempty"` // per-leg entry→exit marks at close
	Fills         int           `json:"fills"`
	RealizedGrid  float64       `json:"realized_grid"`
	FillLog       []gridFill    `json:"fill_log,omitempty"`
}

var (
	pgridMu    sync.Mutex
	pgridStore []protectGridRecord
	pgridFile  string
)

func initProtectGrids(dataDir string) {
	pgridMu.Lock()
	defer pgridMu.Unlock()
	pgridFile = filepath.Join(dataDir, "protect_grids.json")
	b, err := os.ReadFile(pgridFile)
	if err == nil {
		_ = json.Unmarshal(b, &pgridStore)
	}
}

func persistProtectGrids() {
	if pgridFile == "" {
		return
	}
	b, _ := json.MarshalIndent(pgridStore, "", "  ")
	_ = os.WriteFile(pgridFile, b, 0600)
}

func saveProtectGridRecord(rec protectGridRecord) {
	pgridMu.Lock()
	defer pgridMu.Unlock()
	for i := range pgridStore {
		if pgridStore[i].ID == rec.ID {
			pgridStore[i] = rec
			persistProtectGrids()
			return
		}
	}
	pgridStore = append(pgridStore, rec)
	persistProtectGrids()
}

func pgridByID(id string) (protectGridRecord, bool) {
	pgridMu.Lock()
	defer pgridMu.Unlock()
	for _, g := range pgridStore {
		if g.ID == id {
			return g, true
		}
	}
	return protectGridRecord{}, false
}

func openProtectGrids() []protectGridRecord {
	pgridMu.Lock()
	defer pgridMu.Unlock()
	out := []protectGridRecord{}
	for _, g := range pgridStore {
		if g.Status == "OPEN" {
			out = append(out, g)
		}
	}
	return out
}

// pgridAnchor resolves the working ladder anchor (trailing). Records opened
// before trailing existed carry Anchor=0 and fall back to the entry spot.
func pgridAnchor(g *protectGridRecord) float64 {
	if g.Anchor > 0 {
		return g.Anchor
	}
	return g.EntrySpot
}

// pgridNextRung is the nearest ladder rung on the working side of the
// anchor: LONG buys only above it, SHORT sells only below it. Pure.
func pgridNextRung(g *protectGridRecord) float64 {
	a := pgridAnchor(g)
	if g.Direction == gridShort {
		return a - g.GridStep
	}
	return a + g.GridStep
}

// pgridRung is one ladder level for the profile chart.
type pgridRung struct {
	Price float64 `json:"price"`
}

// pgridLotMark is one open futures lot with its take-profit level.
type pgridLotMark struct {
	Entry float64 `json:"entry"`
	TP    float64 `json:"tp"`
	Qty   int     `json:"qty"`
}

// pgridChart is everything the profile chart needs on one price axis:
// the protective wing's expiry payoff AND its current (BS) P&L plus the grid
// layout (rungs, open lots with TPs) and markers (spot, entry, anchor,
// strike).
type pgridChart struct {
	Spots      []float64          `json:"spots"`
	WingExpiry []float64          `json:"wing_expiry"`
	WingNow    []float64          `json:"wing_now"`
	WingNowPnl float64            `json:"wing_now_pnl"`
	IsCall     bool               `json:"is_call"`
	Rungs      []float64          `json:"rungs"`
	Lots       []pgridLotMark     `json:"lots"`
	Markers    map[string]float64 `json:"markers"`
}

// buildPgridChart composes the profile chart payload. LONG rungs sit above
// the anchor, SHORT below; the wing is the long option's expiry payoff plus
// its BS value now at ivAnnual (flat smile) minus entry, ×mult×qty — a long
// option's time value keeps WingNow above WingExpiry. Pure — unit-tested.
func buildPgridChart(direction string, strike, optEntry float64, isCall bool, optQty int, mult, anchor, step, spot, entry float64, maxInv int, lots []pgridLotMark, ivAnnual, tYears float64) pgridChart {
	ch := pgridChart{Markers: map[string]float64{}, IsCall: isCall}
	if step <= 0 || maxInv <= 0 {
		return ch
	}
	for k := 1; k <= maxInv; k++ {
		if direction == gridShort {
			ch.Rungs = append(ch.Rungs, anchor-float64(k)*step)
		} else {
			ch.Rungs = append(ch.Rungs, anchor+float64(k)*step)
		}
	}
	ch.Lots = lots
	if ch.Lots == nil {
		ch.Lots = []pgridLotMark{}
	}
	ch.Markers["spot"] = spot
	ch.Markers["entry"] = entry
	ch.Markers["anchor"] = anchor
	ch.Markers["strike"] = strike
	// X range covers every drawn element with padding.
	lo, hi := math.Inf(1), math.Inf(-1)
	consider := func(v float64) {
		if v > 0 && !math.IsInf(v, 0) {
			lo = math.Min(lo, v)
			hi = math.Max(hi, v)
		}
	}
	consider(spot)
	consider(entry)
	consider(anchor)
	consider(strike)
	for _, r := range ch.Rungs {
		consider(r)
	}
	for _, l := range lots {
		consider(l.Entry)
		consider(l.TP)
	}
	if math.IsInf(lo, 1) || hi <= lo {
		return ch
	}
	pad := (hi - lo) * 0.15
	if pad <= 0 {
		pad = hi * 0.01
	}
	lo, hi = lo-pad, hi+pad
	// The wing's working side must stay on screen: a put earns left of the
	// strike, a call right of it — give it one full ladder span so the kink
	// never sits glued to the edge.
	if span := step * float64(maxInv); span > 0 {
		if isCall {
			hi += span
		} else {
			lo -= span
		}
	}
	const n = 61
	priceNow := func(s float64) (float64, bool) {
		if ivAnnual <= 0.02 || tYears <= 0 {
			return 0, false
		}
		t := tYears
		if t < 1.0/3650.0 {
			t = 1.0 / 3650.0
		}
		g := quant.CalculateBlackScholes(isCall, s, strike, t, 0.16, ivAnnual)
		return (g.Price - optEntry) * mult * float64(optQty), true
	}
	for i := 0; i < n; i++ {
		s := lo + (hi-lo)*float64(i)/float64(n-1)
		var intr float64
		if isCall {
			intr = math.Max(s-strike, 0)
		} else {
			intr = math.Max(strike-s, 0)
		}
		pnl := (intr - optEntry) * mult * float64(optQty)
		ch.Spots = append(ch.Spots, math.Round(s*100)/100)
		ch.WingExpiry = append(ch.WingExpiry, math.Round(pnl*100)/100)
		if v, ok := priceNow(s); ok {
			ch.WingNow = append(ch.WingNow, math.Round(v*100)/100)
		}
	}
	if len(ch.WingNow) == n {
		if v, ok := priceNow(spot); ok {
			ch.WingNowPnl = math.Round(v*100) / 100
		}
	} else {
		ch.WingNow = nil
	}
	return ch
}

// clearProtectGridRecords wipes the whole registry (test-mode reset).
// Returns the number of removed records.
func clearProtectGridRecords() int {
	pgridMu.Lock()
	defer pgridMu.Unlock()
	n := len(pgridStore)
	pgridStore = nil
	persistProtectGrids()
	return n
}

func allProtectGrids() []protectGridRecord {
	pgridMu.Lock()
	defer pgridMu.Unlock()
	out := make([]protectGridRecord, len(pgridStore))
	copy(out, pgridStore)
	return out
}

// isProtectGridPositionID reports whether a position belongs to a protective
// grid (hidden from the central dashboard lists, lives on its own tab).
func isProtectGridPositionID(id string) bool {
	pgridMu.Lock()
	defer pgridMu.Unlock()
	for _, g := range pgridStore {
		if g.PositionID == id {
			return true
		}
	}
	return false
}

// isProtectGridTrade reports protective-grid journal entries.
func isProtectGridTrade(strategy string) bool {
	return strings.Contains(strategy, "Protective Grid")
}

// resolveProtectStrike picks the 1-step-OTM strike for the wing: the strike
// just below spot for PUT protection (LONG grid), just above for CALL
// (SHORT grid). Pure.
func resolveProtectStrike(strikes []float64, spot float64, direction string) float64 {
	if len(strikes) == 0 || spot <= 0 {
		return 0
	}
	sorted := append([]float64{}, strikes...)
	sort.Float64s(sorted)
	if direction == gridLong {
		best := 0.0
		for _, s := range sorted {
			if s < spot {
				best = s
			} else {
				break
			}
		}
		return best
	}
	for _, s := range sorted {
		if s > spot {
			return s
		}
	}
	return 0
}

// protectGridPlan is the pre-trade economics shown by the plan endpoint.
type protectGridPlan struct {
	Symbol              string         `json:"symbol"`
	Direction           string         `json:"direction"`
	Expiry              string         `json:"expiry"`
	DaysToExp           int            `json:"days_to_exp"`
	EntrySpot           float64        `json:"entry_spot"`
	ProtectStrike       float64        `json:"protect_strike"`
	ProtectIsCall       bool           `json:"protect_is_call"`
	ProtectQty          int            `json:"protect_qty"`
	ProtectSecID        string         `json:"protect_secid"`
	PremiumEach         float64        `json:"premium_each"`
	PremiumTotal        float64        `json:"premium_total"`
	ThetaDay            float64        `json:"theta_day"`
	DeltaEach           float64        `json:"delta_each"`
	GridStep            float64        `json:"grid_step"`
	TakeProfit          float64        `json:"take_profit"`
	QtyPerLevel         int            `json:"qty_per_level"`
	MaxInventory        int            `json:"max_inventory"`
	BreakevenRoundTrips *float64       `json:"breakeven_roundtrips_per_day"`
	Coverage            float64        `json:"coverage_ratio"`
	MaxGridLoss         float64        `json:"max_grid_loss_rub"`
	Scenarios           []gridScenario `json:"scenarios"`
	Signal              gridSignal     `json:"signal"`
	Warnings            []string       `json:"warnings"`
}

type gridScenario struct {
	Name       string  `json:"name"`
	GridPnL    float64 `json:"grid_pnl"`
	Inventory  int     `json:"inventory"`
	Unreal     float64 `json:"unrealized"`
	Fills      int     `json:"fills"`
	MaxAdverse int     `json:"max_adverse_rub"`
	Verdict    string  `json:"verdict"`
}

// buildProtectGridPlan prices the wing + grid economics. Option pricing uses
// the hybrid optionMark (live narrow books at mid, dead books at BS fair) so
// a 500-wide dead spread never prints a fantasy premium. Pure apart from the
// mark/discovery feeds.
func buildProtectGridPlan(symbol, direction, expiry string, spot float64, protectQty int, step, tpPts float64, qtyPerLevel, maxInv int, feePerFill float64) (*protectGridPlan, error) {
	if direction != gridLong && direction != gridShort {
		return nil, fmt.Errorf("направление: LONG или SHORT")
	}
	if spot <= 0 || isEstimatePrice(spot) {
		return nil, fmt.Errorf("нет живого спота — оценка запрещена")
	}
	strikes, findOpt, err := optionChainFor(symbol, expiry)
	if err != nil || len(strikes) == 0 {
		return nil, fmt.Errorf("нет цепочки страйков на %s", expiry)
	}
	strike := resolveProtectStrike(strikes, spot, direction)
	if strike <= 0 {
		return nil, fmt.Errorf("нет страйка на шаг от спота %.0f", spot)
	}
	isCall := direction == gridShort
	opt := findOpt(strike, isCall)
	if opt == nil {
		return nil, fmt.Errorf("нет опциона на страйке %g", strike)
	}
	dte := dteInDays(expiry, time.Now())
	if dte <= 0 {
		return nil, fmt.Errorf("серия истекла")
	}
	t := float64(dte) / 365.0
	mult := contractMultiplier(symbol)
	px := optionMark(opt.SecID, isCall, strike, spot, t, symbol, expiry)
	if px <= 0 {
		return nil, fmt.Errorf("нет цены защиты (стакан пуст)")
	}
	if protectQty < 1 {
		protectQty = 1
	}
	if qtyPerLevel < 1 {
		qtyPerLevel = 1
	}
	if maxInv < 1 {
		maxInv = 5
	}
	if step <= 0 {
		if symbol == "Si" {
			step = 25
		} else {
			step = 100
		}
	}
	if tpPts <= 0 {
		tpPts = step
	}
	iv := quant.ImpliedVolatility(isCall, px, spot, strike, t, 0.16)
	if iv <= 0.02 {
		iv = 0.30
	}
	g := quant.CalculateBlackScholes(isCall, spot, strike, t, 0.16, iv)
	premiumTotal := math.Round(px*mult*float64(protectQty)*100) / 100
	thetaDay := math.Round(g.Theta*mult*float64(protectQty)*100) / 100 // negative for long
	thetaAbs := -thetaDay
	if thetaAbs < 0 {
		thetaAbs = 0
	}
	plan := &protectGridPlan{
		Symbol: symbol, Direction: direction, Expiry: expiry, DaysToExp: dte,
		EntrySpot: spot, ProtectStrike: strike, ProtectIsCall: isCall,
		ProtectQty: protectQty, ProtectSecID: opt.SecID,
		PremiumEach: math.Round(px*100) / 100, PremiumTotal: premiumTotal,
		ThetaDay: thetaDay, DeltaEach: g.Delta,
		GridStep: step, TakeProfit: tpPts, QtyPerLevel: qtyPerLevel, MaxInventory: maxInv,
		BreakevenRoundTrips: finiteOrNil(gridBreakevenRoundTripsPerDay(thetaAbs, tpPts, mult, feePerFill)),
	}
	cov := math.Abs(g.Delta) * float64(protectQty) / float64(maxInv)
	plan.Coverage = math.Round(cov*100) / 100
	// Worst ladder water: full inventory bought, spot keeps running against
	// the grid by stopSpan points with no take profits.
	stopSpan := step * float64(maxInv)
	plan.MaxGridLoss = math.Round(stopSpan*mult*float64(maxInv)*100) / 100
	// Scenarios: 4 tape characters × grid P&L (option leg excluded — its
	// expiry payoff rides in analytics; here the question is "does the grid
	// pay theta").
	amp := step * 6
	if atr := planATRHint(symbol); atr > 0 {
		amp = atr / 4
	}
	scenDefs := []struct{ key, name string }{
		{"chop", "пила ±¼ ATR (базовый сценарий)"},
		{"flat", "флэт (смерть сетки)"},
		{"trend_up", "тренд вверх"},
		{"trend_down", "тренд вниз"},
	}
	for _, sd := range scenDefs {
		path := synthPath(sd.key, spot, 160, amp)
		real, unreal, fills, inv, adv := simulateGrid(direction, path, spot, step, tpPts, mult, feePerFill, qtyPerLevel, maxInv)
		verdict := "сетка окупает тету"
		if real < thetaAbs {
			verdict = "сетка НЕ окупает дневную тету"
		}
		if sd.key == "flat" && fills == 0 {
			verdict = "нет заливок — тета вхолостую"
		}
		plan.Scenarios = append(plan.Scenarios, gridScenario{
			Name: sd.name, GridPnL: real, Inventory: inv, Unreal: unreal, Fills: fills, MaxAdverse: adv, Verdict: verdict,
		})
	}
	if cov < 0.5 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("слабое покрытие: опционы закрывают лишь %.0f%% макс. инвентаря — при продолжении тренда сетка потечёт", cov*100))
	}
	if tpPts*mult <= 2*feePerFill {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("тейк %.0f не покрывает 2 комиссии (%.0f ₽): КАЖДЫЙ круг сетки в минус — увеличь тейк минимум до %.0f", tpPts, 2*feePerFill, 2*feePerFill/mult+1))
	}
	if dte < 14 {
		plan.Warnings = append(plan.Warnings, "DTE < 14: тета-ускорение и гамма-взрыв у экспирации — только докатка, не новый вход")
	}
	if dte > 45 {
		plan.Warnings = append(plan.Warnings, "DTE > 45: премия дорогая, тета медленная — защита переплачена")
	}
	return plan, nil
}

// planATRHint returns a fast ATR hint for scenario amplitude (cached range
// history, no network in tests via override).
var planATRHintOverride = 0.0

func planATRHint(symbol string) float64 {
	if planATRHintOverride > 0 {
		return planATRHintOverride
	}
	return 0
}

// ---- HTTP API ----

// GET /api/v1/protect-grid/signal?symbol=Si — live exhaustion read from the
// range tracker (no trade).
func protectGridSignalHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	today, err := fetchRangeToday(symbol)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	if today.Stale || today.ATR14 <= 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "торги закрыты или нет ATR — сигнал недоступен"})
		return
	}
	sig := protectGridSignal(today.Open, today.High, today.Low, today.Last, today.ATR14)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol": symbol, "today": today, "signal": sig,
	})
}

// POST /api/v1/protect-grid/plan — pre-trade economics.
func protectGridPlanHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Symbol       string  `json:"symbol"`
		Direction    string  `json:"direction"`
		Expiry       string  `json:"expiry"`
		ProtectQty   int     `json:"protect_qty"`
		GridStep     float64 `json:"grid_step"`
		TakeProfit   float64 `json:"take_profit"`
		QtyPerLevel  int     `json:"qty_per_level"`
		MaxInventory int     `json:"max_inventory"`
		FeePerFill   float64 `json:"fee_per_fill"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Symbol == "" {
		req.Symbol = "Si"
	}
	if req.Direction == "" {
		req.Direction = gridLong
	}
	req.Direction = strings.ToUpper(req.Direction)
	if req.ProtectQty < 1 {
		req.ProtectQty = 10
	}
	if req.FeePerFill <= 0 {
		req.FeePerFill = 4
	}
	if req.Expiry == "" {
		today := time.Now().Format("2006-01-02")
		best := ""
		bestDTE := 1 << 30
		for _, s := range optionSeriesForSymbol(req.Symbol) {
			if s.LastDelDate < today {
				continue
			}
			d := dteInDays(s.LastDelDate, time.Now())
			if d >= 14 && d < bestDTE {
				bestDTE, best = d, s.LastDelDate
			}
		}
		if best == "" {
			for _, s := range optionSeriesForSymbol(req.Symbol) {
				if s.LastDelDate >= today {
					best = s.LastDelDate
					break
				}
			}
		}
		req.Expiry = best
	}
	spot, _ := getSpotPrice(req.Symbol)
	plan, err := buildProtectGridPlan(req.Symbol, req.Direction, req.Expiry, spot, req.ProtectQty, req.GridStep, req.TakeProfit, req.QtyPerLevel, req.MaxInventory, req.FeePerFill)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	// Attach the live exhaustion read so the plan says WHEN, not just WHAT.
	if t, terr := fetchRangeToday(req.Symbol); terr == nil && !t.Stale && t.ATR14 > 0 {
		plan.Signal = protectGridSignal(t.Open, t.High, t.Low, t.Last, t.ATR14)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "plan": plan})
}

// POST /api/v1/protect-grid/open — buy the wing + start the ladder.
func protectGridOpenHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Symbol       string  `json:"symbol"`
		Direction    string  `json:"direction"`
		Expiry       string  `json:"expiry"`
		ProtectQty   int     `json:"protect_qty"`
		GridStep     float64 `json:"grid_step"`
		TakeProfit   float64 `json:"take_profit"`
		QtyPerLevel  int     `json:"qty_per_level"`
		MaxInventory int     `json:"max_inventory"`
		FeePerFill   float64 `json:"fee_per_fill"`
		MaxLossRub   float64 `json:"max_loss_rub"`
		ProfitTarget float64 `json:"profit_target_rub"`
		TimeStopDTE  int     `json:"time_stop_dte"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Symbol == "" {
		req.Symbol = "Si"
	}
	req.Direction = strings.ToUpper(req.Direction)
	if req.Direction == "" {
		req.Direction = gridLong
	}
	if req.ProtectQty < 1 {
		req.ProtectQty = 10
	}
	if req.QtyPerLevel < 1 {
		req.QtyPerLevel = 1
	}
	if req.MaxInventory < 1 {
		req.MaxInventory = 5
	}
	if req.FeePerFill <= 0 {
		req.FeePerFill = 4
	}
	if req.TimeStopDTE <= 0 {
		req.TimeStopDTE = 7
	}
	spot, _ := getSpotPrice(req.Symbol)
	if spot <= 0 || isEstimatePrice(spot) {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "нет живого спота — вход по оценке запрещён"})
		return
	}
	if req.Expiry == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "укажи экспирацию (см. /api/v1/protect-grid/plan)"})
		return
	}
	plan, err := buildProtectGridPlan(req.Symbol, req.Direction, req.Expiry, spot, req.ProtectQty, req.GridStep, req.TakeProfit, req.QtyPerLevel, req.MaxInventory, req.FeePerFill)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	// A take profit below two fees loses on every circle by construction —
	// refuse instead of opening a guaranteed bleeder (same strictness as
	// the no-estimate rule).
	if plan.TakeProfit*contractMultiplier(req.Symbol) <= 2*req.FeePerFill {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false,
			"error": fmt.Sprintf("тейк %.0f не покрывает 2 комиссии (%.0f ₽): каждый круг в минус — поставь тейк ≥ %.0f", plan.TakeProfit, 2*req.FeePerFill, 2*req.FeePerFill/contractMultiplier(req.Symbol)+1)})
		return
	}
	// Executable wing fill only: BUY at the ask touch.
	wingFill, err := futuresFillPrice(plan.ProtectSecID, "BUY")
	if err != nil {
		// futuresFillPrice reads futures books; options need the ask side.
		// Fall back to the hybrid mark only when it came from a live book.
		if q, ok := cachedOptionQuoteEx(plan.ProtectSecID); ok && q.Offer > 0 && q.Bid > 0 && q.Offer >= q.Bid {
			wingFill = q.Offer
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "нет живого аска защиты: " + err.Error()})
			return
		}
	}
	// Futures contract for the ladder: front contract.
	futSec := ""
	if alorMarket != nil {
		if syms, serr := alorMarket.FetchOptionChain(req.Symbol); serr == nil {
			futSec = resolveFuturesAlor(syms, req.Symbol, time.Now())
		}
	}
	if futSec == "" {
		futSec = selectedSeriesFor(req.Symbol)
		if isSyntheticSeriesCode(futSec) {
			futSec = resolveRealFuturesCode(req.Symbol, futSec)
		}
	}
	if futSec == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "нет фьючерса для сетки"})
		return
	}
	mult := contractMultiplier(req.Symbol)
	p := quant.Position{
		ID:       fmt.Sprintf("pos-%d", time.Now().UnixNano()/1e6),
		Strategy: "Protective Grid",
		Symbol:   req.Symbol,
		Expiry:   req.Expiry,
		OpenedAt: time.Now(),
		Margin:   math.Round(wingFill*mult*float64(req.ProtectQty)*100) / 100,
	}
	p.Legs = append(p.Legs, quant.PositionLeg{
		SecID: plan.ProtectSecID, Symbol: req.Symbol, Kind: "OPTION",
		Side: "BUY", Quantity: req.ProtectQty, Strike: plan.ProtectStrike,
		IsCall: plan.ProtectIsCall, EntryPrice: wingFill, CurrentPrice: wingFill,
	})
	repricePosition(&p)
	quant.SavePosition(p)

	rec := protectGridRecord{
		ID: recID("pgrid"), PositionID: p.ID,
		Symbol: req.Symbol, Direction: req.Direction, Expiry: req.Expiry,
		EntrySpot: spot, Anchor: spot, ProtectSecID: plan.ProtectSecID, ProtectStrike: plan.ProtectStrike,
		ProtectIsCall: plan.ProtectIsCall, ProtectQty: req.ProtectQty, ProtectEntry: wingFill,
		EntryValue: math.Round(p.EntryValue*100) / 100,
		GridStep:   plan.GridStep, TakeProfit: plan.TakeProfit, QtyPerLevel: req.QtyPerLevel,
		MaxInventory: req.MaxInventory, FeePerFill: req.FeePerFill, FuturesSecID: futSec,
		MaxLossRub: req.MaxLossRub, ProfitTarget: req.ProfitTarget, TimeStopDTE: req.TimeStopDTE,
		Status: "OPEN", OpenedAt: time.Now().Format(time.RFC3339),
		FillLog: []gridFill{{At: time.Now().Format("02.01 15:04"), Side: "BUY", Qty: req.ProtectQty, Price: wingFill, Kind: "OPEN", Note: fmt.Sprintf("защита %s %.0f", map[bool]string{true: "CALL", false: "PUT"}[plan.ProtectIsCall], plan.ProtectStrike)}},
	}
	saveProtectGridRecord(rec)
	logTelegramErr("pgrid-open", sendTelegramMessage(
		fmt.Sprintf("🛡 Защитная сетка %s %s открыта: %s %.0f ×%d по %.0f, сетка %s шаг %.0f/тейк %.0f, макс. %d фьюч",
			telegramEscape(req.Symbol), telegramEscape(rec.ID),
			map[bool]string{true: "CALL", false: "PUT"}[plan.ProtectIsCall],
			plan.ProtectStrike, req.ProtectQty, wingFill,
			req.Direction, plan.GridStep, plan.TakeProfit, req.MaxInventory)))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "grid": rec, "plan": plan})
}

// GET /api/v1/protect-grid — open grids with live P&L.
func protectGridListHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := []map[string]interface{}{}
	for _, g := range openProtectGrids() {
		item := map[string]interface{}{
			"id": g.ID, "symbol": g.Symbol, "direction": g.Direction, "expiry": g.Expiry,
			"entry_spot": g.EntrySpot, "protect_strike": g.ProtectStrike, "protect_qty": g.ProtectQty,
			"grid_step": g.GridStep, "take_profit": g.TakeProfit, "max_inventory": g.MaxInventory,
			"status": g.Status, "opened_at": g.OpenedAt, "fills": g.Fills,
			"dte": dteInDays(g.Expiry, time.Now()),
		}
		if pos, found := quant.GetPositionByID(g.PositionID); found {
			repricePosition(pos)
			quant.SavePosition(*pos)
			item["pnl"] = math.Round(pos.PnL*100) / 100
			item["realized"] = math.Round(pos.RealizedPnL*100) / 100
			item["inventory"] = gridOpenInventory(pos.Legs, g.Direction)
			item["net_delta"] = math.Round(pos.Delta*100) / 100
			item["theta_day"] = math.Round(pos.Theta*100) / 100
			item["margin"] = math.Round(pos.Margin)
		} else {
			item["note"] = "позиция не найдена"
		}
		out = append(out, item)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"grids": out})
}

// gridExitLeg freezes one leg's entry→exit marks at close, so "at what price
// did the options close" is always answerable from the record itself.
type gridExitLeg struct {
	SecID  string  `json:"secid"`
	Kind   string  `json:"kind"`
	Side   string  `json:"side"`
	Qty    int     `json:"qty"`
	Strike float64 `json:"strike,omitempty"`
	IsCall bool    `json:"is_call,omitempty"`
	Entry  float64 `json:"entry"`
	Exit   float64 `json:"exit"`
}

// gridStatRow is one grid's summary for the stats table.
type gridStatRow struct {
	ID           string  `json:"id"`
	Symbol       string  `json:"symbol"`
	Direction    string  `json:"direction"`
	Status       string  `json:"status"`
	Expiry       string  `json:"expiry"`
	OpenedAt     string  `json:"opened_at"`
	ClosedAt     string  `json:"closed_at,omitempty"`
	CloseReason  string  `json:"close_reason,omitempty"`
	EntryValue   float64 `json:"entry_value,omitempty"`
	ExitValue    float64 `json:"exit_value,omitempty"`
	Pnl          float64 `json:"pnl"`
	PnlKnown     bool    `json:"pnl_known"` // false when neither record nor journal knows the final
	RealizedGrid float64 `json:"realized_grid"`
	Fills        int     `json:"fills"`
	Takes        int     `json:"takes"`    // closed circles (TAKE fills)
	TakePnl      float64 `json:"take_pnl"` // Σ closed P&L of takes, ex-fees
	Fees         float64 `json:"fees"`
}

// protectGridStats aggregates the whole grid-trading history.
type protectGridStats struct {
	Total        int           `json:"total"`
	Open         int           `json:"open"`
	Closed       int           `json:"closed"`
	Wins         int           `json:"wins"`
	WinRate      float64       `json:"win_rate"`
	TotalPnl     float64       `json:"total_pnl"`
	RealizedGrid float64       `json:"realized_grid"`
	Takes        int           `json:"takes"`
	AvgTake      float64       `json:"avg_take"`
	Fills        int           `json:"fills"`
	Fees         float64       `json:"fees"`
	ThetaDayOpen float64       `json:"theta_day_open"`
	Rows         []gridStatRow `json:"rows"`
}

// matchGridTrade finds the journal trade of a closed grid record: same symbol
// and strategy, opened within ±10 minutes of the record (the journal carries
// no grid id). used guards against double-assigning one trade. Pure —
// unit-tested. Heals records closed before finals were stored.
func matchGridTrade(rec protectGridRecord, trades []quant.Trade, used map[string]bool) *quant.Trade {
	opened, err := time.Parse(time.RFC3339, rec.OpenedAt)
	if err != nil {
		return nil
	}
	var best *quant.Trade
	bestDiff := 11 * time.Minute
	for i := range trades {
		t := &trades[i]
		if t.Strategy != "Protective Grid" || t.Symbol != rec.Symbol || used[t.ID] {
			continue
		}
		d := t.OpenedAt.Sub(opened)
		if d < 0 {
			d = -d
		}
		if d <= 10*time.Minute && d < bestDiff {
			best, bestDiff = t, d
		}
	}
	return best
}

// aggregateProtectGridStats folds grid records into trading stats. Live P&L +
// theta arrive via the lookup (keyed by PositionID), journal trades heal
// records closed before finals were stored — the function stays pure,
// unit-tested.
func aggregateProtectGridStats(records []protectGridRecord, live map[string]struct{ Pnl, Theta float64 }, trades []quant.Trade) protectGridStats {
	st := protectGridStats{}
	usedTrade := map[string]bool{}
	for _, g := range records {
		row := gridStatRow{
			ID: g.ID, Symbol: g.Symbol, Direction: g.Direction, Status: g.Status,
			Expiry: g.Expiry, OpenedAt: g.OpenedAt, ClosedAt: g.ClosedAt,
			CloseReason: g.CloseReason, EntryValue: g.EntryValue,
			RealizedGrid: math.Round(g.RealizedGrid*100) / 100, Fills: g.Fills,
		}
		for _, f := range g.FillLog {
			if f.Kind == "TAKE" {
				row.Takes++
				row.TakePnl += f.ClosedPnl
			}
		}
		row.TakePnl = math.Round(row.TakePnl*100) / 100
		row.Fees = math.Round(float64(g.Fills)*g.FeePerFill*100) / 100
		if g.Status == "OPEN" {
			st.Open++
			if l, ok := live[g.PositionID]; ok {
				row.Pnl, row.PnlKnown = math.Round(l.Pnl*100)/100, true
				st.ThetaDayOpen += l.Theta
			}
		} else {
			st.Closed++
			if g.ClosedAt != "" {
				row.Pnl, row.PnlKnown = g.FinalPnl, true
				if len(g.ExitLegs) > 0 {
					exit := 0.0
					for _, l := range g.ExitLegs {
						dir := 1.0
						if l.Side == "SELL" {
							dir = -1
						}
						exit += dir * l.Exit * contractMultiplier(g.Symbol) * float64(l.Qty)
					}
					row.ExitValue = math.Round(exit*100) / 100
				}
			} else if tr := matchGridTrade(g, trades, usedTrade); tr != nil {
				usedTrade[tr.ID] = true
				row.Pnl = math.Round(tr.RealizedPnL*100) / 100
				row.PnlKnown = true
				row.EntryValue = math.Round(tr.EntryValue*100) / 100
				row.ExitValue = math.Round(tr.ExitValue*100) / 100
				row.CloseReason = "из журнала (закрыта до учёта причин)"
			}
		}
		if row.PnlKnown {
			st.TotalPnl += row.Pnl
		}
		st.Total++
		st.RealizedGrid += row.RealizedGrid
		st.Takes += row.Takes
		st.Fills += row.Fills
		st.Fees += row.Fees
		st.Rows = append(st.Rows, row)
	}
	// Win rate counts CLOSED grids with a known final only.
	known, wins := 0, 0
	for _, r := range st.Rows {
		if r.Status == "OPEN" || !r.PnlKnown {
			continue
		}
		known++
		if r.Pnl > 0 {
			wins++
		}
	}
	st.Wins = wins
	if known > 0 {
		st.WinRate = math.Round(float64(wins) / float64(known) * 1000 / 10)
	}
	st.TotalPnl = math.Round(st.TotalPnl*100) / 100
	st.RealizedGrid = math.Round(st.RealizedGrid*100) / 100
	st.Fees = math.Round(st.Fees*100) / 100
	st.ThetaDayOpen = math.Round(st.ThetaDayOpen*100) / 100
	if st.Takes > 0 {
		takeSum := 0.0
		for _, r := range st.Rows {
			takeSum += r.TakePnl
		}
		st.AvgTake = math.Round(takeSum/float64(st.Takes)*100) / 100
	}
	if st.Rows == nil {
		st.Rows = []gridStatRow{}
	}
	return st
}

// GET /api/v1/protect-grid/stats — aggregate grid-trading statistics.
func protectGridStatsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	live := map[string]struct{ Pnl, Theta float64 }{}
	for _, g := range openProtectGrids() {
		if pos, found := quant.GetPositionByID(g.PositionID); found {
			repricePosition(pos)
			quant.SavePosition(*pos)
			live[g.PositionID] = struct{ Pnl, Theta float64 }{pos.PnL + pos.RealizedPnL, pos.Theta}
		}
	}
	json.NewEncoder(w).Encode(aggregateProtectGridStats(allProtectGrids(), live, quant.GetTrades()))
}

// GET /api/v1/protect-grid/analytics?id=pgrid-... — state, scenario matrix,
// breakeven and fill journal.
func protectGridAnalyticsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := r.URL.Query().Get("id")
	g, found := pgridByID(id)
	if !found {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "grid not found"})
		return
	}
	if g.Status != "OPEN" {
		// The position is gone — render the frozen close snapshot: per-leg
		// entry→exit, final, reason and the fill journal.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"closed": true, "grid": g,
			"final_pnl":    g.FinalPnl,
			"close_reason": g.CloseReason,
			"exit_legs":    g.ExitLegs,
			"fill_log":     g.FillLog,
		})
		return
	}
	pos, ok := quant.GetPositionByID(g.PositionID)
	if !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "position not found"})
		return
	}
	repricePosition(pos)
	quant.SavePosition(*pos)
	mult := contractMultiplier(pos.Symbol)
	spot, spotSuspect := analyticsSpot(pos.Legs, pos.Symbol)
	if spot <= 0 {
		spot, _ = getSpotPrice(pos.Symbol)
		spotSuspect = true
	}
	dte := dteInDays(g.Expiry, time.Now())
	inv := gridOpenInventory(pos.Legs, g.Direction)
	target := gridLadderTarget(g.Direction, pgridAnchor(&g), spot, g.GridStep, g.MaxInventory)
	closable := gridTakeProfitLegs(pos.Legs, g.Direction, spot, g.TakeProfit)
	// Option expiry payoff at ±2 steps (per-share → rubles via mult).
	payoff := []map[string]interface{}{}
	for _, bump := range []float64{-2, -1, -0.5, 0, 0.5, 1, 2} {
		px := spot + bump*g.GridStep*float64(g.MaxInventory)
		var v float64
		if g.ProtectIsCall {
			v = math.Max(px-g.ProtectStrike, 0) - g.ProtectEntry
		} else {
			v = math.Max(g.ProtectStrike-px, 0) - g.ProtectEntry
		}
		payoff = append(payoff, map[string]interface{}{
			"spot": math.Round(px), "wing_pnl": math.Round(v*mult*float64(g.ProtectQty)*100) / 100,
		})
	}
	scenarios := []gridScenario{}
	amp := g.GridStep * 6
	for _, sd := range []struct{ key, name string }{
		{"chop", "пила"}, {"flat", "флэт"}, {"trend_up", "тренд вверх"}, {"trend_down", "тренд вниз"},
	} {
		path := synthPath(sd.key, spot, 160, amp)
		real, unreal, fills, invS, adv := simulateGrid(g.Direction, path, g.EntrySpot, g.GridStep, g.TakeProfit, mult, g.FeePerFill, g.QtyPerLevel, g.MaxInventory)
		scenarios = append(scenarios, gridScenario{Name: sd.name, GridPnL: real, Inventory: invS, Unreal: unreal, Fills: fills, MaxAdverse: adv})
	}
	lots := []pgridLotMark{}
	for _, l := range pos.Legs {
		if l.Kind != "FUTURES" || l.Quantity <= 0 {
			continue
		}
		tp := l.EntryPrice + g.TakeProfit
		if g.Direction == gridShort {
			tp = l.EntryPrice - g.TakeProfit
		}
		lots = append(lots, pgridLotMark{Entry: l.EntryPrice, TP: tp, Qty: l.Quantity})
	}
	ivAnnual, tYears := 0.0, float64(dte)/365.0
	for _, l := range pos.Legs {
		if l.Kind != "OPTION" || l.CurrentPrice <= 0 || l.Strike <= 0 || spot <= 0 || tYears <= 0 {
			continue
		}
		if iv := quant.ImpliedVolatility(l.IsCall, l.CurrentPrice, spot, l.Strike, tYears, 0.16); iv > 0.02 && iv <= 3 {
			ivAnnual = iv
		}
		break
	}
	chart := buildPgridChart(g.Direction, g.ProtectStrike, g.ProtectEntry, g.ProtectIsCall,
		g.ProtectQty, mult, pgridAnchor(&g), g.GridStep, spot, g.EntrySpot, g.MaxInventory, lots, ivAnnual, tYears)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"grid":                         g,
		"spot":                         math.Round(spot*100) / 100,
		"chart":                        chart,
		"pnl":                          math.Round(pos.PnL*100) / 100,
		"realized":                     math.Round(pos.RealizedPnL*100) / 100,
		"inventory":                    inv,
		"target":                       target,
		"anchor":                       math.Round(pgridAnchor(&g)*100) / 100,
		"next_rung":                    math.Round(pgridNextRung(&g)*100) / 100,
		"closable_tp":                  closable,
		"net_delta":                    math.Round(pos.Delta*100) / 100,
		"theta_day":                    math.Round(pos.Theta*100) / 100,
		"margin":                       math.Round(pos.Margin),
		"breakeven_roundtrips_per_day": finiteOrNil(gridBreakevenRoundTripsPerDay(math.Abs(pos.Theta), g.TakeProfit, mult, g.FeePerFill)),
		"wing_payoff_expiry":           payoff,
		"scenarios":                    scenarios,
		"dte":                          dte,
		"spot_suspect":                 spotSuspect,
		"fill_log":                     g.FillLog,
	})
}

// POST /api/v1/protect-grid/close {"id":"pgrid-..."} — manual close.
func protectGridCloseHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	g, found := pgridByID(req.ID)
	if !found || g.Status != "OPEN" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "grid not found or not open"})
		return
	}
	pos, ok := quant.GetPositionByID(g.PositionID)
	if !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "position not found"})
		return
	}
	closeProtectGrid(&g, pos, "закрыта вручную", true)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func closeProtectGrid(g *protectGridRecord, pos *quant.Position, reason string, notify bool) {
	repricePosition(pos)
	quant.SavePosition(*pos) // persist fresh marks so exit legs are exact
	removed, found := quant.RemovePosition(pos.ID)
	if !found {
		return
	}
	tr := quant.SettleTrade(removed)
	enrichTradeContext(&tr, g.Symbol, g.Expiry, g.EntrySpot)
	quant.AddTrade(tr)
	g.Status = "CLOSED"
	g.ClosedAt = time.Now().Format(time.RFC3339)
	g.CloseReason = reason
	g.FinalPnl = math.Round((removed.PnL+removed.RealizedPnL)*100) / 100
	g.ExitLegs = nil
	for _, l := range removed.Legs {
		g.ExitLegs = append(g.ExitLegs, gridExitLeg{
			SecID: l.SecID, Kind: l.Kind, Side: l.Side, Qty: l.Quantity,
			Strike: l.Strike, IsCall: l.IsCall,
			Entry: math.Round(l.EntryPrice*100) / 100,
			Exit:  math.Round(l.CurrentPrice*100) / 100,
		})
	}
	saveProtectGridRecord(*g)
	if notify {
		logTelegramErr("pgrid-close", sendTelegramMessage(
			fmt.Sprintf("🛡 Сетка %s %s закрыта: %s\nP&L %s ₽ (сетка реализовано %s ₽, заливок %d)",
				telegramEscape(g.Symbol), telegramEscape(g.ID), telegramEscape(reason),
				formatRub(removed.PnL+removed.RealizedPnL, 0), formatRub(g.RealizedGrid, 0), g.Fills)))
	}
}

// ---- Manager (60s loop, paper fills at executable touches) ----

var (
	pgridManagerMu sync.Mutex
	pgridManagerOn bool
)

func startProtectGridManager() {
	pgridManagerMu.Lock()
	if pgridManagerOn {
		pgridManagerMu.Unlock()
		return
	}
	pgridManagerOn = true
	pgridManagerMu.Unlock()
	go func() {
		for {
			time.Sleep(60 * time.Second)
			if marketHalt() {
				continue
			}
			runProtectGridPass()
		}
	}()
}

// pgridEval is one loop decision. Pure apart from inputs.
type pgridEval struct {
	Action string // CLOSE_STOP | CLOSE_TIME | CLOSE_TP | NONE
	Reason string
}

func evaluateProtectGrid(g *protectGridRecord, pnl float64, dte int) pgridEval {
	if g.MaxLossRub > 0 && pnl <= -g.MaxLossRub {
		return pgridEval{"CLOSE_STOP", fmt.Sprintf("стоп %.0f ₽: P&L %s ₽", g.MaxLossRub, formatRub(pnl, 0))}
	}
	limit := g.TimeStopDTE
	if limit <= 0 {
		limit = 7
	}
	if dte <= limit {
		return pgridEval{"CLOSE_TIME", fmt.Sprintf("time-stop: DTE %d ≤ %d", dte, limit)}
	}
	if g.ProfitTarget > 0 && pnl >= g.ProfitTarget {
		return pgridEval{"CLOSE_TP", fmt.Sprintf("тейк %.0f ₽: P&L %s ₽", g.ProfitTarget, formatRub(pnl, 0))}
	}
	return pgridEval{"NONE", ""}
}

func runProtectGridPass() {
	now := time.Now()
	for _, g := range openProtectGrids() {
		pos, ok := quant.GetPositionByID(g.PositionID)
		if !ok {
			continue
		}
		repricePosition(pos)
		quant.SavePosition(*pos)
		spot, _ := getSpotPrice(pos.Symbol)
		if spot <= 0 || isEstimatePrice(spot) {
			continue // freeze on ghosts — no ladder moves on fake spots
		}
		dte := dteInDays(g.Expiry, now)
		total := pos.PnL + pos.RealizedPnL
		if ev := evaluateProtectGrid(&g, total, dte); ev.Action != "NONE" {
			closeProtectGrid(&g, pos, ev.Reason, true)
			continue
		}
		mult := contractMultiplier(pos.Symbol)
		// 1) Take profits: close touched ladder legs via FIFO netting.
		if qty := gridTakeProfitLegs(pos.Legs, g.Direction, spot, g.TakeProfit); qty > 0 {
			side := "SELL"
			if g.Direction == gridShort {
				side = "BUY"
			}
			if err := pgridExecuteLadder(&g, pos, side, qty, spot, mult, "TAKE", "тейк-профит юнита"); err != nil {
				log.Printf("pgrid %s take failed: %v", g.ID, err)
			} else {
				// Reload after execution for the ladder step below.
				if p2, ok2 := quant.GetPositionByID(g.PositionID); ok2 {
					pos = p2
				}
				if g2, ok2 := pgridByID(g.ID); ok2 {
					g = g2
				}
			}
		}
		// 2) Trail a fully loaded ladder behind a runaway price, then top up
		// toward the target from the anchor (cap 3 lots/pass so a gap never
		// opens the whole book at once). Fixed rungs re-arm themselves on
		// pullback-and-rise; nothing slides while flat.
		inv := gridOpenInventory(pos.Legs, g.Direction)
		anchor := pgridAnchor(&g)
		if trailed := trailGridAnchor(g.Direction, anchor, spot, g.GridStep, g.MaxInventory, inv); trailed != anchor {
			g.Anchor = trailed
			saveProtectGridRecord(g)
		}
		target := gridLadderTarget(g.Direction, pgridAnchor(&g), spot, g.GridStep, g.MaxInventory)
		if target > inv {
			add := target - inv
			if add > 3 {
				add = 3
			}
			side := "BUY"
			if g.Direction == gridShort {
				side = "SELL"
			}
			if err := pgridExecuteLadder(&g, pos, side, add*g.QtyPerLevel, spot, mult, "LADDER", fmt.Sprintf("лестница к цели %d", target)); err != nil {
				log.Printf("pgrid %s ladder failed: %v", g.ID, err)
			}
		}
	}
}

// pgridExecuteLadder nets/appends paper futures legs at the executable touch,
// updates counters + journal. Shared by the loop (take + ladder).
func pgridExecuteLadder(g *protectGridRecord, pos *quant.Position, side string, qty int, spot, mult float64, kind, note string) error {
	if qty <= 0 {
		return nil
	}
	if g.FuturesSecID == "" {
		return fmt.Errorf("нет фьючерса для сетки")
	}
	fill, err := futuresFillPrice(g.FuturesSecID, side)
	if err != nil {
		return err
	}
	legs, realized, residual := quant.NetFuturesLegs(pos.Legs, g.FuturesSecID, side, qty, fill, mult)
	pos.Legs = legs
	pos.RealizedPnL += realized
	g.RealizedGrid += realized
	if residual > 0 {
		pos.Legs = append(pos.Legs, quant.PositionLeg{
			SecID: g.FuturesSecID, Symbol: pos.Symbol, Kind: "FUTURES",
			Side: side, Quantity: residual, EntryPrice: fill, CurrentPrice: fill,
		})
		pos.Margin += fill * mult * 0.15 * float64(residual)
	}
	repricePosition(pos)
	quant.SavePosition(*pos)
	g.Fills++
	g.FillLog = append(g.FillLog, gridFill{
		At: time.Now().Format("02.01 15:04"), Side: side, Qty: qty,
		Price: fill, Kind: kind, ClosedPnl: math.Round(realized*100) / 100, Note: note,
	})
	if len(g.FillLog) > 100 {
		g.FillLog = g.FillLog[len(g.FillLog)-100:]
	}
	saveProtectGridRecord(*g)
	return nil
}

// POST /api/v1/protect-grid/clear {"close_open":true,"clear_journal":true} —
// test-mode reset: close every OPEN grid, then wipe all records (open and
// closed) and, on request, the module's journal trades. Closes are quiet
// (no Telegram) to avoid spamming one receipt per grid.
func protectGridClearHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		CloseOpen    *bool `json:"close_open"`
		ClearJournal *bool `json:"clear_journal"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	closeOpen, clearJournal := true, true
	if req.CloseOpen != nil {
		closeOpen = *req.CloseOpen
	}
	if req.ClearJournal != nil {
		clearJournal = *req.ClearJournal
	}
	closed, orphaned := 0, 0
	if closeOpen {
		for _, g := range openProtectGrids() {
			pos, ok := quant.GetPositionByID(g.PositionID)
			if !ok {
				g.Status = "CLOSED"
				g.ClosedAt = time.Now().Format(time.RFC3339)
				g.CloseReason = "очистка (позиция не найдена)"
				saveProtectGridRecord(g)
				orphaned++
				continue
			}
			closeProtectGrid(&g, pos, "очистка модуля", false)
			closed++
		}
	}
	removed := clearProtectGridRecords()
	journal := 0
	if clearJournal {
		journal = quant.RemoveTradesByStrategy("Protective Grid")
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "closed": closed, "orphaned": orphaned,
		"records_removed": removed, "journal_removed": journal,
	})
}

// GET /api/v1/protect-grid/manager — status stub (log lives in fill journals).
func protectGridManagerHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled": true, "interval_sec": 60, "open": len(openProtectGrids()),
	})
}
