package main

// Short-straddle module (second trading module): sell an ATM straddle on high
// IV Rank and keep it delta-neutral with an automatic futures hedger.
//
// Two constructions (chosen at open):
//   - naked:   SELL ATM call + SELL ATM put (max GO, no futures leg);
//   - covered: naked legs + LONG futures (starts ~+1.0 delta per unit; the
//     broker-side SPAN offset is what lowers the real GO — our estimate
//     stays conservative).
//
// Four hedge rules (per position, evaluated on every pass):
//   - delta_band: hedge when |Δ| ≥ band (flatten to zero);
//   - time:       flatten whatever delta every intervalMin minutes;
//   - hybrid:     time checks + immediate flatten on big moves (|Δ| ≥
//     bigMoveMult × band);
//   - price_band: flatten when spot moved ≥ priceBandPct from the last hedge.
//
// Guardrails (paper-first): stop at 2× collected premium, time-stop at
// timeStopDTE (default 14). Core math is pure and hermetic; market prices
// arrive via injected pricers so tests need no network.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"option-quant-ai/alor"
	"option-quant-ai/quant"
)

// Hedge rule identifiers.
const (
	hedgeDeltaBand = "delta_band"
	hedgeTime      = "time"
	hedgeHybrid    = "hybrid"
	hedgePriceBand = "price_band"
)

// straddleHedgeRules configures one position's hedger. Zero values mean
// "use the defaults" (resolved by resolveHedgeRules).
type straddleHedgeRules struct {
	Rule         string  `json:"rule"` // delta_band | time | hybrid | price_band
	DeltaBand    float64 `json:"delta_band"`
	IntervalMin  int     `json:"interval_min"`
	BigMoveMult  float64 `json:"big_move_mult"`
	PriceBandPct float64 `json:"price_band_pct"`
}

// resolveHedgeRules fills zero values with defaults.
func resolveHedgeRules(r straddleHedgeRules) straddleHedgeRules {
	if r.Rule == "" {
		r.Rule = hedgeHybrid
	}
	if r.DeltaBand <= 0 {
		r.DeltaBand = 1.0
	}
	if r.IntervalMin <= 0 {
		r.IntervalMin = 60
	}
	if r.BigMoveMult <= 0 {
		r.BigMoveMult = 2.0
	}
	if r.PriceBandPct <= 0 {
		r.PriceBandPct = 1.0
	}
	return r
}

// decideStraddleHedge is the pure hedge engine: given the current position
// delta and the target delta (in futures-contract equivalents, same
// convention as the spread manager — 0 for naked, +Qty to maintain the
// futures cover), spot, last hedge mark and elapsed minutes, it reports
// whether to hedge now, the signed futures qty (toward target, rounded,
// dust skipped), and a human reason. Pure — unit-tested.
//
// Covered positions hold +Qty futures by design (covered call + short put),
// so the engine maintains that cover instead of flattening it away on the
// first pass.
func decideStraddleHedge(posDelta, targetDelta, spot, lastHedgeSpot float64, minutesSinceHedge float64, rules straddleHedgeRules) (bool, int, string) {
	r := resolveHedgeRules(rules)
	dev := posDelta - targetDelta
	qty := int(math.Round(targetDelta - posDelta))
	if qty == 0 {
		return false, 0, ""
	}
	need := func(reason string) (bool, int, string) { return true, qty, reason }
	switch r.Rule {
	case hedgeTime:
		if minutesSinceHedge >= float64(r.IntervalMin) {
			return need(fmt.Sprintf("время: %0.0f мин ≥ %d", minutesSinceHedge, r.IntervalMin))
		}
		return false, 0, ""
	case hedgePriceBand:
		if spot <= 0 {
			return false, 0, ""
		}
		if lastHedgeSpot <= 0 {
			return need("нет точки отсчёта — первый хедж")
		}
		move := math.Abs(spot-lastHedgeSpot) / lastHedgeSpot * 100
		if move >= r.PriceBandPct {
			return need(fmt.Sprintf("цена ушла %.2f%% ≥ %.2f%%", move, r.PriceBandPct))
		}
		return false, 0, ""
	case hedgeHybrid:
		big := math.Abs(dev) >= r.BigMoveMult*r.DeltaBand
		if big {
			return need(fmt.Sprintf("резкое движение: отклонение %0.2f ≥ %.1f× полоса", math.Abs(dev), r.BigMoveMult))
		}
		if minutesSinceHedge >= float64(r.IntervalMin) {
			return need(fmt.Sprintf("время: %0.0f мин ≥ %d", minutesSinceHedge, r.IntervalMin))
		}
		return false, 0, ""
	default: // hedgeDeltaBand
		if math.Abs(dev) >= r.DeltaBand {
			return need(fmt.Sprintf("отклонение Δ %0.2f ≥ полоса %0.2f", math.Abs(dev), r.DeltaBand))
		}
		return false, 0, ""
	}
}

// straddleLeg is one leg of a short-straddle plan/position.
type straddleLeg struct {
	SecID    string  `json:"secid"`
	Side     string  `json:"side"` // SELL (options) | BUY (futures cover)
	Strike   float64 `json:"strike,omitempty"`
	IsCall   bool    `json:"is_call,omitempty"`
	IsFuture bool    `json:"is_future,omitempty"`
	Price    float64 `json:"price"`
	Qty      int     `json:"qty"`
}

// straddlePlan is the full economics of a short straddle before opening.
type straddlePlan struct {
	Symbol      string        `json:"symbol"`
	Expiry      string        `json:"expiry"`
	DaysToExp   int           `json:"days_to_exp"`
	Spot        float64       `json:"spot"`
	Strike      float64       `json:"strike"`
	Qty         int           `json:"qty"`
	WithFutures bool          `json:"with_futures"`
	FuturesSec  string        `json:"futures_secid"`
	Legs        []straddleLeg `json:"legs"`
	NetCredit   float64       `json:"net_credit"` // premium collected, rubles
	StopLevel   float64       `json:"stop_level"` // 2× premium, rubles
	MarginEst   float64       `json:"margin_est"` // conservative GO estimate, rubles
}

// straddlePricer returns executable prices: call/put mids (or fill levels)
// and the futures price. Injected so the builder stays hermetic.
type straddlePricer func(callSecID, putSecID, futuresSecID string) (callPx, putPx, futPx float64, err error)

// buildShortStraddle prices a short ATM straddle: SELL call + SELL put at the
// given strike, optionally plus a LONG futures cover. Prices come from pricer;
// economics follow the same money conventions as spread plans (NetCredit>0).
// Classic covered straddle (Fidelity): covered call + short put, starts
// ~+1.0 delta per unit — bullish tilt, double loss below the strike.
func buildShortStraddle(symbol, expiry string, daysToExp int, spot, strike float64, qty int, withFutures bool, callSecID, putSecID, futuresSecID string, pricer straddlePricer) (*straddlePlan, error) {
	if qty < 1 {
		qty = 1
	}
	if strike <= 0 {
		return nil, fmt.Errorf("нужен страйк ATM")
	}
	callPx, putPx, futPx, err := pricer(callSecID, putSecID, futuresSecID)
	if err != nil {
		return nil, err
	}
	if callPx <= 0 || putPx <= 0 {
		return nil, fmt.Errorf("нет цен ног (call %.2f, put %.2f)", callPx, putPx)
	}
	mult := contractMultiplier(symbol)
	plan := &straddlePlan{
		Symbol: symbol, Expiry: expiry, DaysToExp: daysToExp,
		Spot: spot, Strike: strike, Qty: qty,
		WithFutures: withFutures, FuturesSec: futuresSecID,
	}
	plan.Legs = append(plan.Legs,
		straddleLeg{SecID: callSecID, Side: "SELL", Strike: strike, IsCall: true, Price: callPx, Qty: qty},
		straddleLeg{SecID: putSecID, Side: "SELL", Strike: strike, IsCall: false, Price: putPx, Qty: qty},
	)
	credit := (callPx + putPx) * mult * float64(qty)
	if withFutures {
		if futPx <= 0 {
			return nil, fmt.Errorf("нет цены фьючерса для покрытия")
		}
		plan.Legs = append(plan.Legs,
			straddleLeg{SecID: futuresSecID, Side: "BUY", IsFuture: true, Price: futPx, Qty: qty})
	}
	plan.NetCredit = math.Round(credit*100) / 100
	plan.StopLevel = math.Round(credit*2*100) / 100
	// Conservative GO estimate: twice the collected premium (undefined risk
	// has no wing to anchor on). A futures cover lowers the REAL broker GO
	// via SPAN offsets — the estimate intentionally ignores that.
	plan.MarginEst = math.Round(credit*2*100) / 100
	return plan, nil
}

// Straddle construction identifiers.
const (
	straddleClassic   = "classic"   // SELL call + SELL put (+ LONG futures if covered)
	straddleCovered   = "covered"   // classic + LONG futures cover
	straddleSynthetic = "synthetic" // SELL 2× call + LONG futures (delta-neutral at entry)
)

// buildSyntheticStraddle prices the synthetic short straddle: SELL 2× ATM
// call + LONG futures. By put-call parity (+1F = +1C − 1P) this replicates
// SELL call + SELL put, but starts delta-neutral (≈ −1.0 + 1.0) instead of
// the classic covered +1.0 tilt. Same stop/margin conventions as classic.
func buildSyntheticStraddle(symbol, expiry string, daysToExp int, spot, strike float64, qty int, callSecID, futuresSecID string, pricer straddlePricer) (*straddlePlan, error) {
	if qty < 1 {
		qty = 1
	}
	if strike <= 0 {
		return nil, fmt.Errorf("нужен страйк ATM")
	}
	callPx, _, futPx, err := pricer(callSecID, "", futuresSecID)
	if err != nil {
		return nil, err
	}
	if callPx <= 0 {
		return nil, fmt.Errorf("нет цены колла (%.2f)", callPx)
	}
	if futPx <= 0 {
		return nil, fmt.Errorf("нет цены фьючерса для синтетики")
	}
	mult := contractMultiplier(symbol)
	plan := &straddlePlan{
		Symbol: symbol, Expiry: expiry, DaysToExp: daysToExp,
		Spot: spot, Strike: strike, Qty: qty,
		WithFutures: true, FuturesSec: futuresSecID,
	}
	plan.Legs = append(plan.Legs,
		straddleLeg{SecID: callSecID, Side: "SELL", Strike: strike, IsCall: true, Price: callPx, Qty: 2 * qty},
		straddleLeg{SecID: futuresSecID, Side: "BUY", IsFuture: true, Price: futPx, Qty: qty},
	)
	credit := 2 * callPx * mult * float64(qty)
	plan.NetCredit = math.Round(credit*100) / 100
	plan.StopLevel = math.Round(credit*2*100) / 100
	plan.MarginEst = math.Round(credit*2*100) / 100
	return plan, nil
}

// hedgeTargetDelta returns the delta the hedger maintains: covered classic
// holds its +Qty futures cover, everything else (naked classic, synthetic)
// flattens to zero. Legacy records without Construction fall back to
// WithFutures. Pure.
func hedgeTargetDelta(rec *straddleRecord) float64 {
	if rec == nil {
		return 0
	}
	covered := rec.Construction == straddleCovered ||
		(rec.Construction == "" && rec.WithFutures)
	if !covered {
		return 0
	}
	q := float64(rec.Qty)
	if q < 1 {
		q = 1
	}
	return q
}

// straddleRecord is a live paper short straddle with its hedge state.
type straddleRecord struct {
	ID            string             `json:"id"`
	PositionID    string             `json:"position_id"`
	Symbol        string             `json:"symbol"`
	Expiry        string             `json:"expiry"`
	Strike        float64            `json:"strike"`
	Qty           int                `json:"qty"`
	Construction  string             `json:"construction"` // classic | covered | synthetic
	WithFutures   bool               `json:"with_futures"`
	FuturesSecID  string             `json:"futures_secid,omitempty"`
	NetCredit     float64            `json:"net_credit"`
	StopLevel     float64            `json:"stop_level"`
	Hedge         straddleHedgeRules `json:"hedge"`
	TimeStopDTE   int                `json:"time_stop_dte"`
	Status        string             `json:"status"` // OPEN / CLOSED
	OpenedAt      string             `json:"opened_at"`
	HedgeCount    int                `json:"hedge_count"`
	LastHedgeAt   string             `json:"last_hedge_at"`
	LastHedgeSpot float64            `json:"last_hedge_spot"`
}

// straddleShouldStop reports whether the realized loss hit the 2× stop.
func straddleShouldStop(rec *straddleRecord, pnl float64) bool {
	if rec == nil || rec.StopLevel <= 0 {
		return false
	}
	return pnl <= -rec.StopLevel
}

// straddleShouldTimeStop reports whether the series aged into the time-stop.
func straddleShouldTimeStop(rec *straddleRecord, dte int) bool {
	if rec == nil {
		return false
	}
	limit := rec.TimeStopDTE
	if limit <= 0 {
		limit = 14
	}
	return dte <= limit
}

// minutesSince parses an RFC3339 mark into minutes elapsed (huge when empty).
func minutesSince(mark string, now time.Time) float64 {
	if mark == "" {
		return 1e9
	}
	t, err := time.Parse(time.RFC3339, mark)
	if err != nil {
		return 1e9
	}
	return now.Sub(t).Minutes()
}

// ---- Store (JSON file, mirrors the spreads registry) ----

var (
	straddleMu    sync.Mutex
	straddleStore []straddleRecord
	straddleFile  string
)

func initStraddles(dataDir string) {
	straddleMu.Lock()
	defer straddleMu.Unlock()
	straddleFile = filepath.Join(dataDir, "straddles.json")
	b, err := os.ReadFile(straddleFile)
	if err == nil {
		_ = json.Unmarshal(b, &straddleStore)
	}
}

func persistStraddles() {
	if straddleFile == "" {
		return
	}
	b, _ := json.MarshalIndent(straddleStore, "", "  ")
	_ = os.WriteFile(straddleFile, b, 0600)
}

func saveStraddleRecord(rec straddleRecord) {
	straddleMu.Lock()
	defer straddleMu.Unlock()
	for i := range straddleStore {
		if straddleStore[i].ID == rec.ID {
			straddleStore[i] = rec
			persistStraddles()
			return
		}
	}
	straddleStore = append(straddleStore, rec)
	persistStraddles()
}

func straddleByID(id string) (straddleRecord, bool) {
	straddleMu.Lock()
	defer straddleMu.Unlock()
	for _, s := range straddleStore {
		if s.ID == id {
			return s, true
		}
	}
	return straddleRecord{}, false
}

func openStraddles() []straddleRecord {
	straddleMu.Lock()
	defer straddleMu.Unlock()
	out := []straddleRecord{}
	for _, s := range straddleStore {
		if s.Status == "OPEN" {
			out = append(out, s)
		}
	}
	return out
}

// ---- Alor pricer ----

// sellPriceFromBook picks the executable SELL price from a book: mid when
// two-sided, else best bid (conservative and real — a sale fills at the
// bid). Evening one-sided books no longer block opening. Pure —
// unit-tested.
func sellPriceFromBook(ob alor.AlorOrderbookResponse) (float64, bool) {
	if len(ob.Bids) == 0 || ob.Bids[0].Price <= 0 {
		return 0, false
	}
	bid := ob.Bids[0].Price
	if len(ob.Asks) > 0 && ob.Asks[0].Price >= bid && ob.Asks[0].Price > 0 {
		return (bid + ob.Asks[0].Price) / 2, true
	}
	return bid, true
}

// alorStraddlePricer prices straddle legs from live Alor books.
func alorStraddlePricer(callSecID, putSecID, futuresSecID string) (float64, float64, float64, error) {
	if alorMarket == nil {
		return 0, 0, 0, fmt.Errorf("alor не настроен")
	}
	priceLeg := func(secid, name string) (float64, error) {
		if secid == "" {
			return 0, nil // unneeded leg (e.g. no put in synthetic)
		}
		ob, err := alorMarket.FetchOrderbook("MOEX", secid)
		if err != nil {
			return 0, fmt.Errorf("стакан %s (%s): %v", name, secid, err)
		}
		px, ok := sellPriceFromBook(ob)
		if !ok {
			return 0, fmt.Errorf("в стакане %s (%s) нет бидов", name, secid)
		}
		return px, nil
	}
	callPx, err := priceLeg(callSecID, "call")
	if err != nil {
		return 0, 0, 0, err
	}
	putPx, err := priceLeg(putSecID, "put")
	if err != nil {
		return 0, 0, 0, err
	}
	futPx := 0.0
	if futuresSecID != "" {
		if q, err := alorMarket.FetchSecurityQuote(futuresSecID); err == nil && q.Price > 0 {
			futPx = q.Price
		} else {
			ob, err := alorMarket.FetchOrderbook("MOEX", futuresSecID)
			if err == nil && len(ob.Bids) > 0 && len(ob.Asks) > 0 && ob.Asks[0].Price >= ob.Bids[0].Price {
				futPx = (ob.Bids[0].Price + ob.Asks[0].Price) / 2
			}
		}
	}
	return callPx, putPx, futPx, nil
}

// ---- HTTP API ----

// POST /api/v1/straddles/open {"symbol":"Si","expiry":"2026-12-17","strike":86000,
// "qty":1,"construction":"covered","call_secid":"...","put_secid":"...","futures_secid":"...",
// "hedge":{"rule":"hybrid",...},"time_stop_dte":14}
// construction: classic (C+P) | covered (C+P+F) | synthetic (2C+F).
// with_futures is kept for backward compat (true → covered).
func straddleOpenHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Symbol       string             `json:"symbol"`
		Expiry       string             `json:"expiry"`
		Strike       float64            `json:"strike"`
		Qty          int                `json:"qty"`
		Construction string             `json:"construction"`
		WithFutures  bool               `json:"with_futures"`
		CallSecID    string             `json:"call_secid"`
		PutSecID     string             `json:"put_secid"`
		FuturesSecID string             `json:"futures_secid"`
		Hedge        straddleHedgeRules `json:"hedge"`
		TimeStopDTE  int                `json:"time_stop_dte"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Symbol == "" {
		req.Symbol = "Si"
	}
	construction := req.Construction
	if construction == "" {
		if req.WithFutures {
			construction = straddleCovered
		} else {
			construction = straddleClassic
		}
	}
	needsFutures := construction == straddleCovered || construction == straddleSynthetic
	// Futures auto-resolve: empty secid must not open a zero-price leg —
	// resolve the front contract or refuse with guidance.
	if needsFutures && req.FuturesSecID == "" {
		if alorMarket == nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "укажи secid фьючерса (Alor не настроен)"})
			return
		}
		syms, err := alorMarket.FetchOptionChain(req.Symbol)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "не нашёл фьючерсы в Alor: " + err.Error()})
			return
		}
		if code := resolveFuturesAlor(syms, req.Symbol, time.Now()); code != "" {
			req.FuturesSecID = code
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "не нашёл фьючерс — укажи secid вручную (напр. SiZ6)"})
			return
		}
	}
	// Option legs auto-resolve: exact MOEX pair wins; anything else is
	// refused (guessing call/put opens wrong trades) with guidance.
	if req.Strike > 0 && (req.CallSecID == "" || (construction != straddleSynthetic && req.PutSecID == "")) {
		calls, puts, _, _, source := discoverStraddleLegs(req.Symbol, req.Strike, req.Expiry)
		if source != "moex" || len(calls) != 1 || (construction != straddleSynthetic && len(puts) != 1) {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false,
				"error": "не нашёл ноги точно — укажи secid вручную (MOEX недоступен или пары нет)"})
			return
		}
		req.CallSecID = calls[0].SecID
		if construction != straddleSynthetic {
			req.PutSecID = puts[0].SecID
		}
	}
	spot, _ := getSpotPrice(req.Symbol)
	withFutures := construction == straddleCovered
	var plan *straddlePlan
	var err error
	if construction == straddleSynthetic {
		plan, err = buildSyntheticStraddle(req.Symbol, req.Expiry, dteInDays(req.Expiry, time.Now()),
			spot, req.Strike, req.Qty, req.CallSecID, req.FuturesSecID, alorStraddlePricer)
		withFutures = true
	} else {
		plan, err = buildShortStraddle(req.Symbol, req.Expiry, dteInDays(req.Expiry, time.Now()),
			spot, req.Strike, req.Qty, withFutures, req.CallSecID, req.PutSecID, req.FuturesSecID,
			alorStraddlePricer)
	}
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}

	p := quant.Position{
		ID:       fmt.Sprintf("pos-%d", time.Now().UnixNano()/1e6),
		Strategy: "Short Straddle",
		Symbol:   plan.Symbol,
		Expiry:   plan.Expiry,
		OpenedAt: time.Now(),
		Margin:   plan.MarginEst,
	}
	for _, l := range plan.Legs {
		kind := "OPTION"
		if l.IsFuture {
			kind = "FUTURES"
		}
		p.Legs = append(p.Legs, quant.PositionLeg{
			SecID: l.SecID, Symbol: plan.Symbol, Kind: kind,
			Side: l.Side, Quantity: l.Qty, Strike: l.Strike,
			IsCall: l.IsCall, EntryPrice: l.Price, CurrentPrice: l.Price,
		})
	}
	repricePosition(&p)
	quant.SavePosition(p)

	tsd := req.TimeStopDTE
	if tsd <= 0 {
		tsd = 14
	}
	rec := straddleRecord{
		ID: recID("str"), PositionID: p.ID,
		Symbol: plan.Symbol, Expiry: plan.Expiry, Strike: plan.Strike, Qty: plan.Qty,
		Construction: construction, WithFutures: plan.WithFutures,
		FuturesSecID: plan.FuturesSec,
		NetCredit:    plan.NetCredit, StopLevel: plan.StopLevel,
		Hedge: resolveHedgeRules(req.Hedge), TimeStopDTE: tsd,
		Status: "OPEN", OpenedAt: time.Now().Format(time.RFC3339),
	}
	saveStraddleRecord(rec)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "straddle": rec, "plan": plan})
}

// recID mints a unique record id with the given prefix.
func recID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()/1e6)
}

// futuresMonthCodes maps standard MOEX futures month letters to months
// (SiU6 = Sep 2026, SiZ6 = Dec 2026 — same codes the series badge shows).
var futuresMonthCodes = map[byte]time.Month{
	'F': time.January, 'G': time.February, 'H': time.March, 'J': time.April,
	'K': time.May, 'M': time.June, 'N': time.July, 'Q': time.August,
	'U': time.September, 'V': time.October, 'X': time.November, 'Z': time.December,
}

// parseFuturesCode splits "SiU6" into root + expiry month/year. Pure.
func parseFuturesCode(secid string) (root string, year int, month time.Month, ok bool) {
	for _, r := range []string{"Si", "RI"} {
		rest, found := strings.CutPrefix(secid, r)
		if !found || len(rest) != 2 {
			continue
		}
		m, ok := futuresMonthCodes[rest[0]]
		if !ok || rest[1] < '0' || rest[1] > '9' {
			continue
		}
		now := time.Now()
		base := now.Year() - now.Year()%10 + int(rest[1]-'0')
		if base < now.Year() || (base == now.Year() && m < now.Month()) {
			base += 10
		}
		return r, base, m, true
	}
	return "", 0, 0, false
}

// resolveFuturesAlor picks the front futures contract for the symbol from
// live Alor instrument search (^ROOT<month><digit>). Pure date math on top
// of the search list; no expiry guessing beyond standard month codes.
func resolveFuturesAlor(symbols []string, root string, now time.Time) string {
	best := ""
	var bestY int
	var bestM time.Month
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(root) + `([FGHJKMNQUVXZ])(\d)$`)
	for _, s := range symbols {
		m := re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		mo := futuresMonthCodes[m[1][0]]
		y := now.Year() - now.Year()%10 + int(m[2][0]-'0')
		if y < now.Year() || (y == now.Year() && mo < now.Month()) {
			y += 10
		}
		if best == "" || y < bestY || (y == bestY && mo < bestM) {
			best, bestY, bestM = s, y, mo
		}
	}
	return best
}

// tenorLetter maps a DTE to the expiry tenor marker: W (weekly ≤10d),
// M (monthly 11–45d), Q (quarterly >45d).
func tenorLetter(dte int) string {
	switch {
	case dte <= 10:
		return "W"
	case dte <= 45:
		return "M"
	default:
		return "Q"
	}
}

type expiryOption struct {
	Date  string `json:"date"`
	DTE   int    `json:"dte"`
	Tenor string `json:"tenor"`
}

// buildExpiryOptions turns series dates into dated tenor options (pure).
func buildExpiryOptions(dates []string, today time.Time) []expiryOption {
	out := []expiryOption{}
	seen := map[string]bool{}
	for _, d := range dates {
		if seen[d] {
			continue
		}
		seen[d] = true
		dte := dteInDays(d, today)
		if dte < 1 {
			continue
		}
		out = append(out, expiryOption{Date: d, DTE: dte, Tenor: tenorLetter(dte)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

type strikeOption struct {
	Strike  float64 `json:"strike"`
	Label   string  `json:"label"`
	ATM     bool    `json:"atm"`
	Divider bool    `json:"divider"`
}

// buildStrikeOptions turns a strike grid into dropdown rows with a divider
// row glued right above the ATM strike (pure).
func buildStrikeOptions(strikes []float64, atm float64) []strikeOption {
	sorted := append([]float64{}, strikes...)
	sort.Float64s(sorted)
	out := []strikeOption{}
	for _, s := range sorted {
		if s == atm && atm > 0 {
			out = append(out, strikeOption{Label: "━━━ ATM ━━━", Divider: true})
		}
		out = append(out, strikeOption{Strike: s,
			Label: fmt.Sprintf("%g", s), ATM: s == atm && atm > 0})
	}
	return out
}

// parseStrikesFromSymbols extracts the strike grid from Alor instrument
// symbols (^ROOT<digits>, e.g. Si86000BU6). Expiry-agnostic union across
// series — no expiry or call/put claims, so nothing to get wrong. Pure.
func parseStrikesFromSymbols(symbols []string, root string) []float64 {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(root) + `(\d+)`)
	seen := map[float64]bool{}
	out := []float64{}
	for _, s := range symbols {
		m := re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		f, err := strconv.ParseFloat(m[1], 64)
		if err != nil || f <= 0 {
			continue
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Float64s(out)
	return out
}

// discoverLeg is one candidate leg with a live book preview.
type discoverLeg struct {
	SecID string  `json:"secid"`
	Bid   float64 `json:"bid"`
	Ask   float64 `json:"ask"`
	Label string  `json:"label"`
	Kind  string  `json:"kind,omitempty"` // "call" | "put" | "" unknown
}

// parseAlorOptionInfo extracts strike/expiry/kind from a singular Alor
// instrument record with a tolerant key scan (exact field names vary).
// Returns what it could prove; kind needs one agreeing strong signal.
func parseAlorOptionInfo(raw map[string]interface{}) (strike float64, expiry, kind string) {
	get := func(names ...string) interface{} {
		for _, n := range names {
			for k, v := range raw {
				if strings.EqualFold(k, n) {
					return v
				}
			}
		}
		return nil
	}
	num := func(v interface{}) float64 {
		switch n := v.(type) {
		case float64:
			return n
		case string:
			f, _ := strconv.ParseFloat(strings.ReplaceAll(n, ",", "."), 64)
			return f
		}
		return 0
	}
	str := func(v interface{}) string {
		s, _ := v.(string)
		return s
	}
	strike = num(get("strike", "strikeprice", "strike_price"))
	expiry = str(get("expirationdate", "expiration_date", "maturitydate", "maturity_date", "lastdeldate", "expdate"))
	// Call/put signals: explicit words only (first agreement wins). No
	// positional CFI claims — the exact Alor code layout is unverified.
	candidates := []string{
		str(get("optiontype", "option_type", "putcall", "callput", "type", "instrumenttype")),
		str(get("description", "shortname", "name")),
	}
	for _, c := range candidates {
		u := strings.ToUpper(c)
		if strings.Contains(u, "CALL") && !strings.Contains(u, "PUT") {
			return strike, expiry, "call"
		}
		if strings.Contains(u, "PUT") && !strings.Contains(u, "CALL") {
			return strike, expiry, "put"
		}
	}
	return strike, expiry, ""
}

// GET /api/v1/straddles/discover?symbol=Si&strike=86000&expiry=2026-09-24 —
// resolve leg secids without typing them. MOEX chain first (exact, with
// expiry); Alor search fallback (strike-matched candidates with live book
// previews for manual pick). Futures auto-resolved from month codes.
func discoverWithBook(secid string) discoverLeg {
	leg := discoverLeg{SecID: secid}
	if alorMarket == nil {
		return leg
	}
	if ob, err := alorMarket.FetchOrderbook("MOEX", secid); err == nil {
		if len(ob.Bids) > 0 {
			leg.Bid = ob.Bids[0].Price
		}
		if len(ob.Asks) > 0 {
			leg.Ask = ob.Asks[0].Price
		}
	}
	return leg
}

func futuresMonthLabel(code string) string {
	if _, y, m, ok := parseFuturesCode(code); ok {
		return fmt.Sprintf("%s %d", map[time.Month]string{
			time.January: "янв", time.February: "фев", time.March: "мар",
			time.April: "апр", time.May: "май", time.June: "июн",
			time.July: "июл", time.August: "авг", time.September: "сен",
			time.October: "окт", time.November: "ноя", time.December: "дек",
		}[m], y)
	}
	return ""
}

// discoverStraddleLegs resolves leg candidates: MOEX chain first (exact
// call/put at the strike+expiry), Alor search fallback (book previews,
// unverified types). Shared by the discover endpoint and auto-open.
func discoverStraddleLegs(symbol string, strike float64, expiry string) (calls, puts, unknown, futures []discoverLeg, source string) {
	futures = []discoverLeg{}
	if alorMarket != nil {
		if syms, err := alorMarket.FetchOptionChain(symbol); err == nil {
			if code := resolveFuturesAlor(syms, symbol, time.Now()); code != "" {
				f := discoverWithBook(code)
				f.Label = futuresMonthLabel(code)
				futures = append(futures, f)
			}
		}
	}

	// MOEX exact path.
	if strike > 0 {
		if chain := moexOptionsForAsset(symbol, expiry); len(chain) > 0 {
			if strikes, findOpt, err := optionChainFor(symbol, expiry); err == nil && len(strikes) > 0 {
				_ = strikes
				if c := findOpt(strike, true); c != nil {
					leg := discoverWithBook(c.SecID)
					leg.Kind = "call"
					calls = append(calls, leg)
				}
				if p := findOpt(strike, false); p != nil {
					leg := discoverWithBook(p.SecID)
					leg.Kind = "put"
					puts = append(puts, leg)
				}
				return calls, puts, nil, futures, "moex"
			}
		}
	}

	// Alor fallback: strike-matched candidates with book previews.
	calls, puts, unknown = []discoverLeg{}, []discoverLeg{}, []discoverLeg{}
	if alorMarket != nil && strike > 0 {
		syms, err := alorMarket.FetchOptionChain(symbol)
		if err == nil {
			want := fmt.Sprintf("%.0f", strike)
			cands := []string{}
			for _, s := range syms {
				if strings.HasPrefix(s, symbol) && strings.Contains(s, want) && len(cands) < 16 {
					cands = append(cands, s)
				}
			}
			type res struct {
				idx int
				leg discoverLeg
			}
			out := make([]res, len(cands))
			var wg sync.WaitGroup
			for i, secid := range cands {
				wg.Add(1)
				go func(idx int, id string) {
					defer wg.Done()
					leg := discoverWithBook(id)
					leg.Label = id
					if _, _, kind := parseAlorOptionInfo(map[string]interface{}{"shortname": id}); kind != "" {
						leg.Kind = kind
					}
					// Singular instrument record for firmer typing.
					if code, body, err := alorMarket.RawGet("/md/v2/Securities/MOEX/"+id, ""); err == nil && code == 200 {
						var raw map[string]interface{}
						if jerr := json.Unmarshal(body, &raw); jerr == nil {
							if _, _, kind := parseAlorOptionInfo(raw); kind != "" {
								leg.Kind = kind
							}
						}
					}
					out[idx] = res{idx, leg}
				}(i, secid)
			}
			wg.Wait()
			for _, r := range out {
				switch r.leg.Kind {
				case "call":
					calls = append(calls, r.leg)
				case "put":
					puts = append(puts, r.leg)
				default:
					unknown = append(unknown, r.leg)
				}
			}
		}
	}
	return calls, puts, unknown, futures, "alor"
}

func straddleDiscoverHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	var strike float64
	fmt.Sscanf(r.URL.Query().Get("strike"), "%f", &strike)
	expiry := r.URL.Query().Get("expiry")

	calls, puts, unknown, futures, source := discoverStraddleLegs(symbol, strike, expiry)
	out := map[string]interface{}{"source": source, "futures": futures}
	if source == "moex" {
		if len(calls) > 0 {
			out["calls"] = calls
		}
		if len(puts) > 0 {
			out["puts"] = puts
		}
	} else {
		out["calls"] = calls
		out["puts"] = puts
		out["unknown"] = unknown
		out["note"] = "тип опциона из Alor не подтверждён — сверь secid перед открытием"
	}
	json.NewEncoder(w).Encode(out)
}

// GET /api/v1/straddles/strikes?symbol=Si — strike grid from live Alor
// instrument search. Works without MOEX (union across expiries).
func straddleStrikesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	if alorMarket == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"strikes": []float64{}})
		return
	}
	syms, err := alorMarket.FetchOptionChain(symbol)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol": symbol, "strikes": parseStrikesFromSymbols(syms, symbol),
	})
}

// GET /api/v1/straddles/meta?symbol=Si — expiries with W/M/Q tenors, strike
// grid with the ATM divider, live spot. Empty lists when the chain is
// unreachable (the form keeps manual inputs as fallback).
func straddleMetaHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	spot, _ := getSpotPrice(symbol)

	dates := []string{}
	for _, s := range optionSeriesForSymbol(symbol) {
		if s.LastDelDate >= today {
			dates = append(dates, s.LastDelDate)
		}
	}
	expiries := buildExpiryOptions(dates, now)

	// Strikes from the preferred expiry (first ≥14d, else the front one).
	strikes := []float64{}
	pick := ""
	for _, e := range expiries {
		if e.DTE >= 14 {
			pick = e.Date
			break
		}
	}
	if pick == "" && len(expiries) > 0 {
		pick = expiries[0].Date
	}
	if pick != "" {
		if ch, _, err := optionChainFor(symbol, pick); err == nil {
			strikes = ch
		}
	}
	atm := 0.0
	if spot > 0 && len(strikes) > 0 {
		atm = nearestStrikeFromStrikes(strikes, spot)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol": symbol, "spot": math.Round(spot*100) / 100,
		"expiries": expiries, "strikes": buildStrikeOptions(strikes, atm),
		"atm_strike": atm, "chain_expiry": pick,
	})
}

// thetaAccrualCurve projects cumulative time-decay P&L over the coming days
// at a frozen spot: Σ leg BS values at (t−d) minus value now. Short premium
// accrues upward. Pure — unit-tested.
func thetaAccrualCurve(legs []analyticsLeg, spot float64, dte int, mult float64, days int) (xs []float64, cumul []float64) {
	const r = 0.16
	valueAt := func(tYears float64) float64 {
		v := 0.0
		for _, l := range legs {
			if l.Kind == "FUTURES" {
				continue // frozen spot: futures don't decay
			}
			dir := 1.0
			if l.Side == "SELL" {
				dir = -1
			}
			iv := l.Iv / 100.0
			if iv <= 0 {
				iv = 0.30
			}
			t := tYears
			if t <= 0 {
				t = 1.0 / 3650.0
			}
			g := quant.CalculateBlackScholes(l.IsCall, spot, l.Strike, t, r, iv)
			v += dir * g.Price * mult * float64(l.Quantity)
		}
		return v
	}
	t0 := float64(dte) / 365.0
	v0 := valueAt(t0)
	for d := 0; d <= days; d++ {
		xs = append(xs, float64(d))
		cumul = append(cumul, math.Round((valueAt(float64(dte-d)/365.0)-v0)*100)/100)
	}
	return xs, cumul
}

// hedgeSpotForecast finds the nearest spots below/above the current one
// where |delta| first reaches the band, scanning the delta curve outward.
// Returns zeros when the band is never touched in range. Pure.
func hedgeSpotForecast(spots, deltas []float64, curIdx int, band float64) (lo, hi float64) {
	if band <= 0 || curIdx < 0 || curIdx >= len(spots) || len(spots) != len(deltas) {
		return 0, 0
	}
	for i := curIdx; i >= 0; i-- {
		if math.Abs(deltas[i]) >= band {
			lo = spots[i]
			break
		}
	}
	for i := curIdx; i < len(spots); i++ {
		if math.Abs(deltas[i]) >= band {
			hi = spots[i]
			break
		}
	}
	return lo, hi
}

type straddleHedgeView struct {
	Rule          string  `json:"rule"`
	Band          float64 `json:"band"`
	Target        float64 `json:"target"`
	CurrentDelta  float64 `json:"current_delta"`
	SpotLo        float64 `json:"spot_lo"`
	SpotHi        float64 `json:"spot_hi"`
	MinutesToTime int     `json:"minutes_to_time_hedge"`
	Text          string  `json:"text"`
}

// buildHedgeView composes the "when is the first hedge" forecast from the
// rule, the delta curve and the last hedge mark. Deviation is measured from
// the hedge target (covered holds +Qty, rest flatten to zero) — not from
// raw delta. Pure apart from time.
func buildHedgeView(rec *straddleRecord, spots, deltas []float64, curSpot float64, now time.Time) straddleHedgeView {
	r := resolveHedgeRules(rec.Hedge)
	target := hedgeTargetDelta(rec)
	v := straddleHedgeView{Rule: r.Rule, Band: r.DeltaBand, Target: target}
	curIdx := -1
	best := math.MaxFloat64
	for i, s := range spots {
		if d := math.Abs(s - curSpot); d < best {
			best, curIdx = d, i
		}
	}
	// Deviation curve: distance from the maintained target.
	dev := make([]float64, len(deltas))
	for i, d := range deltas {
		dev[i] = d - target
	}
	if curIdx >= 0 && curIdx < len(deltas) {
		v.CurrentDelta = math.Round(deltas[curIdx]*100) / 100
	}
	lo, hi := hedgeSpotForecast(spots, dev, curIdx, r.DeltaBand)
	v.SpotLo, v.SpotHi = math.Round(lo), math.Round(hi)
	left := r.IntervalMin - int(minutesSince(rec.LastHedgeAt, now))
	if left < 0 {
		left = 0
	}
	v.MinutesToTime = left
	// Never hedged: the time rule is due at the first check, not "in 0 min".
	timeNote := fmt.Sprintf("через ~%d мин", left)
	if rec.LastHedgeAt == "" {
		timeNote = "при первой проверке"
	}
	tgtNote := ""
	if target != 0 {
		sign := ""
		if target > 0 {
			sign = "+"
		}
		tgtNote = fmt.Sprintf(", цель %s%0.0f", sign, target)
	}
	switch r.Rule {
	case hedgeTime:
		v.Text = fmt.Sprintf("временной хедж %s (интервал %d%s)", timeNote, r.IntervalMin, tgtNote)
	case hedgePriceBand:
		v.Text = fmt.Sprintf("хедж при уходе спота на %.2f%% от %.0f%s", r.PriceBandPct, curSpot, tgtNote)
	default: // delta_band + hybrid: spot triggers on deviation
		curDev := v.CurrentDelta - target
		// Band already breached: don't print nonsense "≤ X или ≥ X"
		// around the current spot — say hedge is due now.
		if math.Abs(curDev) >= r.DeltaBand {
			v.Text = fmt.Sprintf("отклонение %0.2f уже за полосой %0.2f — хедж сейчас%s", curDev, r.DeltaBand, tgtNote)
			if r.Rule == hedgeHybrid {
				v.Text += fmt.Sprintf(" (либо по времени %s)", timeNote)
			}
			break
		}
		parts := []string{}
		if lo > 0 {
			parts = append(parts, fmt.Sprintf("≤ %.0f", lo))
		}
		if hi > 0 {
			parts = append(parts, fmt.Sprintf("≥ %.0f", hi))
		}
		if len(parts) == 0 {
			v.Text = fmt.Sprintf("полоса |Δ| %.2f в диапазоне кривой не задевается%s", r.DeltaBand, tgtNote)
		} else {
			v.Text = fmt.Sprintf("первый хедж при споте %s (|Δ| %.2f%s)", strings.Join(parts, " или "), r.DeltaBand, tgtNote)
		}
		if r.Rule == hedgeHybrid {
			v.Text += fmt.Sprintf("; либо по времени %s", timeNote)
		}
	}
	return v
}

// GET /api/v1/straddles/analytics?id=str-... — legs, curves, theta accrual,
// hedge forecast.
func straddleAnalyticsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := r.URL.Query().Get("id")
	s, found := straddleByID(id)
	if !found {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "straddle not found"})
		return
	}
	pos, ok := quant.GetPositionByID(s.PositionID)
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
	dte := dteInDays(s.Expiry, time.Now())

	legs := make([]analyticsLeg, 0, len(pos.Legs))
	for _, l := range pos.Legs {
		kind := "OPTION"
		if l.Kind == "FUTURES" {
			kind = "FUTURES"
		}
		leg := analyticsLeg{
			SecID: l.SecID, Side: l.Side, Kind: kind, Strike: l.Strike,
			IsCall: l.IsCall, Quantity: l.Quantity,
			Entry: l.EntryPrice, Current: l.CurrentPrice,
		}
		// Quote provenance per leg: a leg marked at entry price with no
		// live book behind it (dead evening series) must be visibly stale,
		// otherwise its frozen P&L reads as "no move".
		if kind == "OPTION" {
			if q, ok := cachedOptionQuoteEx(l.SecID); ok && l.CurrentPrice > 0 {
				leg.MarkSrc = quoteMarkSrc(q, l.CurrentPrice)
			}
		} else {
			leg.MarkSrc = "spot"
		}
		legs = append(legs, leg)
	}
	a := buildSpreadAnalytics(pos.Symbol, s.Expiry, spot, dte, mult, legs)
	xs, cumul := thetaAccrualCurve(a.Legs, spot, dte, mult, 14)
	hv := buildHedgeView(&s, a.Curves.Spots, a.Curves.DeltaNow, spot, time.Now())
	json.NewEncoder(w).Encode(map[string]interface{}{
		"analytics":     a,
		"theta_accrual": map[string]interface{}{"days": xs, "cumul": cumul},
		"hedge":         hv,
		"pnl":           math.Round(pos.PnL*100) / 100,
		"realized":      math.Round(pos.RealizedPnL*100) / 100,
		"spot_suspect":  spotSuspect,
	})
}

// GET /api/v1/straddles — open straddles with live position P&L.
func straddleListHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := []map[string]interface{}{}
	for _, s := range openStraddles() {
		item := map[string]interface{}{
			"id": s.ID, "symbol": s.Symbol, "expiry": s.Expiry,
			"strike": s.Strike, "qty": s.Qty, "with_futures": s.WithFutures,
			"construction": s.Construction,
			"net_credit":   s.NetCredit, "stop_level": s.StopLevel,
			"hedge_rule": s.Hedge.Rule, "time_stop_dte": s.TimeStopDTE,
			"status": s.Status, "opened_at": s.OpenedAt,
			"hedge_count": s.HedgeCount,
			"dte":         dteInDays(s.Expiry, time.Now()),
		}
		if pos, found := quant.GetPositionByID(s.PositionID); found {
			repricePosition(pos)
			quant.SavePosition(*pos)
			item["pnl"] = math.Round(pos.PnL*100) / 100
			item["entry_value"] = math.Round(pos.EntryValue*100) / 100
			item["current_value"] = math.Round(pos.CurrentValue*100) / 100
			item["net_delta"] = math.Round(pos.Delta*100) / 100
		} else {
			item["note"] = "позиция не найдена"
		}
		out = append(out, item)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"straddles": out})
}

// straddleFuturesSecID resolves the hedge futures contract: the recorded
// cover secid first, else the earliest futures leg in the position.
func straddleFuturesSecID(rec *straddleRecord, pos *quant.Position) string {
	if rec.FuturesSecID != "" {
		return rec.FuturesSecID
	}
	for i := range pos.Legs {
		if pos.Legs[i].Kind == "FUTURES" && pos.Legs[i].SecID != "" {
			return pos.Legs[i].SecID
		}
	}
	return ""
}

// POST /api/v1/straddles/hedge {"id":"str-..."} — one manual hedge step:
// evaluate the record's rule against live delta and append a paper futures
// leg toward the target (covered holds +Qty, rest flatten to zero).
func straddleHedgeHandler(w http.ResponseWriter, r *http.Request) {
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
	s, found := straddleByID(req.ID)
	if !found || s.Status != "OPEN" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "straddle not found or not open"})
		return
	}
	pos, ok := quant.GetPositionByID(s.PositionID)
	if !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "position not found"})
		return
	}
	repricePosition(pos)
	wasDelta := pos.Delta
	spot, _ := getSpotPrice(pos.Symbol)
	target := hedgeTargetDelta(&s)
	now := time.Now()
	fire, qty, reason := decideStraddleHedge(wasDelta, target, spot, s.LastHedgeSpot,
		minutesSince(s.LastHedgeAt, now), s.Hedge)
	if !fire {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "хедж не требуется (Δ в полосе): " + reason})
		return
	}
	side := "BUY"
	if qty < 0 {
		side = "SELL"
		qty = -qty
	}
	if err := executeStraddleHedge(&s, pos, straddleEval{"HEDGE", side, qty, reason}, spot); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "side": side, "qty": qty, "reason": reason})
}

// straddleEval is one loop decision: close (stop/time-stop), hedge, or hold.
type straddleEval struct {
	Action string // "CLOSE_STOP" | "CLOSE_TIME" | "HEDGE" | "NONE"
	Side   string // futures side for HEDGE
	Qty    int    // futures qty for HEDGE
	Reason string
}

// evaluateStraddle applies stops first, then the hedge rule. Pure apart from
// inputs — unit-tested.
func evaluateStraddle(rec *straddleRecord, pnl, delta, spot float64, dte int, now time.Time) straddleEval {
	if straddleShouldStop(rec, pnl) {
		return straddleEval{"CLOSE_STOP", "", 0,
			fmt.Sprintf("стоп 2× премии: P&L %s ₽", formatRub(pnl, 0))}
	}
	if straddleShouldTimeStop(rec, dte) {
		return straddleEval{"CLOSE_TIME", "", 0,
			fmt.Sprintf("time-stop: DTE %d ≤ %d", dte, ifPositive(rec.TimeStopDTE, 14))}
	}
	target := hedgeTargetDelta(rec)
	fire, qty, reason := decideStraddleHedge(delta, target, spot, rec.LastHedgeSpot,
		minutesSince(rec.LastHedgeAt, now), rec.Hedge)
	if !fire {
		return straddleEval{"NONE", "", 0, ""}
	}
	side := "BUY"
	if qty < 0 {
		side = "SELL"
		qty = -qty
	}
	return straddleEval{"HEDGE", side, qty, reason}
}

func ifPositive(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

var (
	straddleManagerMu sync.Mutex
	straddleManagerOn bool
)

// startStraddleManager launches the 60s loop over OPEN paper straddles:
// stops, time-stops, then hedge rules. Live exchange orders are never
// placed (paper only) — fills are recorded at executable touches.
func startStraddleManager() {
	straddleManagerMu.Lock()
	if straddleManagerOn {
		straddleManagerMu.Unlock()
		return
	}
	straddleManagerOn = true
	straddleManagerMu.Unlock()

	go func() {
		for {
			time.Sleep(60 * time.Second)
			runStraddleManagerPass()
		}
	}()
}

// runStraddleManagerPass evaluates every OPEN straddle once.
func runStraddleManagerPass() {
	now := time.Now()
	for _, s := range openStraddles() {
		pos, ok := quant.GetPositionByID(s.PositionID)
		if !ok {
			continue
		}
		repricePosition(pos)
		quant.SavePosition(*pos)
		spot, _ := getSpotPrice(pos.Symbol)
		if isEstimatePrice(spot) {
			// Freeze on the last mark instead of firing price-band hedges
			// off fake data; delta/time rules don't need spot.
			spot = s.LastHedgeSpot
		}
		dte := dteInDays(s.Expiry, now)
		ev := evaluateStraddle(&s, pos.PnL, pos.Delta, spot, dte, now)
		switch ev.Action {
		case "CLOSE_STOP", "CLOSE_TIME":
			closeStraddlePosition(&s, pos, ev.Reason, true)
		case "HEDGE":
			executeStraddleHedge(&s, pos, ev, spot)
		}
	}
}

// closeStraddlePosition closes the position, journals the trade and notifies.
// notify controls the Telegram message (auto closes always notify).
func closeStraddlePosition(s *straddleRecord, pos *quant.Position, reason string, notify bool) {
	repricePosition(pos)
	removed, found := quant.RemovePosition(pos.ID)
	if !found {
		return
	}
	// SettleTrade folds netted-hedge realized P&L in exactly once.
	quant.AddTrade(quant.SettleTrade(removed))
	s.Status = "CLOSED"
	saveStraddleRecord(*s)
	if notify {
		logTelegramErr("straddle-close", sendTelegramMessage(
			fmt.Sprintf("📕 Стрэддл %s %s закрыт: %s\nP&L %s ₽",
				telegramEscape(s.Symbol), telegramEscape(s.ID),
				telegramEscape(reason), formatRub(removed.PnL, 0))))
	}
}

// executeStraddleHedge nets/appends a paper futures hedge leg at the
// executable touch and notifies. Opposite legs net FIFO with realized P&L
// preserved on the position (never lost). Shared by manual endpoint & loop.
func executeStraddleHedge(s *straddleRecord, pos *quant.Position, ev straddleEval, spot float64) error {
	futSec := straddleFuturesSecID(s, pos)
	if futSec == "" {
		return fmt.Errorf("нет фьючерса для хеджа")
	}
	fill, err := futuresFillPrice(futSec, ev.Side)
	if err != nil {
		return fmt.Errorf("хедж невозможен: %v", err)
	}
	mult := contractMultiplier(pos.Symbol)
	legs, realized, residual := quant.NetFuturesLegs(pos.Legs, futSec, ev.Side, ev.Qty, fill, mult)
	pos.Legs = legs
	pos.RealizedPnL += realized
	if residual > 0 {
		pos.Legs = append(pos.Legs, quant.PositionLeg{
			SecID: futSec, Symbol: pos.Symbol, Kind: "FUTURES",
			Side: ev.Side, Quantity: residual, EntryPrice: fill, CurrentPrice: fill,
		})
		pos.Margin += fill * mult * 0.15 * float64(residual)
	}
	// Margin stays conservative on netting (released amount unknown
	// per-leg); broker nets for real, we never understate.
	repricePosition(pos)
	quant.SavePosition(*pos)
	now := time.Now()
	s.HedgeCount++
	s.LastHedgeAt = now.Format(time.RFC3339)
	s.LastHedgeSpot = spot
	saveStraddleRecord(*s)
	msg := fmt.Sprintf("🔧 Стрэддл %s %s: хедж %s %d фьюч (%s)",
		telegramEscape(s.Symbol), telegramEscape(s.ID), ev.Side, ev.Qty, telegramEscape(ev.Reason))
	if realized != 0 {
		msg += fmt.Sprintf(" · закрыто хеджей: %s ₽", formatRub(realized, 0))
	}
	logTelegramErr("straddle-hedge", sendTelegramMessage(msg))
	return nil
}

// POST /api/v1/straddles/close {"id":"str-..."}
func straddleCloseHandler(w http.ResponseWriter, r *http.Request) {
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
	s, found := straddleByID(req.ID)
	if !found || s.Status != "OPEN" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "straddle not found or not open"})
		return
	}
	pos, ok := quant.GetPositionByID(s.PositionID)
	if !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "position not found"})
		return
	}
	closeStraddlePosition(&s, pos, "закрыт вручную", true)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
