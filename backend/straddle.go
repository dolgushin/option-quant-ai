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
// delta (in futures-contract equivalents, same convention as the spread
// manager), spot, last hedge mark and elapsed minutes, it reports whether to
// hedge now, the signed futures qty (flatten to zero, rounded, dust
// skipped), and a human reason. Pure — unit-tested.
func decideStraddleHedge(posDelta, spot, lastHedgeSpot float64, minutesSinceHedge float64, rules straddleHedgeRules) (bool, int, string) {
	r := resolveHedgeRules(rules)
	qty := int(math.Round(-posDelta))
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
		if lastHedgeSpot <= 0 {
			return need("нет точки отсчёта — первый хедж")
		}
		move := math.Abs(spot-lastHedgeSpot) / lastHedgeSpot * 100
		if move >= r.PriceBandPct {
			return need(fmt.Sprintf("цена ушла %.2f%% ≥ %.2f%%", move, r.PriceBandPct))
		}
		return false, 0, ""
	case hedgeHybrid:
		big := math.Abs(posDelta) >= r.BigMoveMult*r.DeltaBand
		if big {
			return need(fmt.Sprintf("резкое движение: |Δ| %0.2f ≥ %.1f× полоса", math.Abs(posDelta), r.BigMoveMult))
		}
		if minutesSinceHedge >= float64(r.IntervalMin) {
			return need(fmt.Sprintf("время: %0.0f мин ≥ %d", minutesSinceHedge, r.IntervalMin))
		}
		return false, 0, ""
	default: // hedgeDeltaBand
		if math.Abs(posDelta) >= r.DeltaBand {
			return need(fmt.Sprintf("|Δ| %0.2f ≥ полоса %0.2f", math.Abs(posDelta), r.DeltaBand))
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

// straddleRecord is a live paper short straddle with its hedge state.
type straddleRecord struct {
	ID            string             `json:"id"`
	PositionID    string             `json:"position_id"`
	Symbol        string             `json:"symbol"`
	Expiry        string             `json:"expiry"`
	Strike        float64            `json:"strike"`
	Qty           int                `json:"qty"`
	WithFutures   bool               `json:"with_futures"`
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
			return 0, fmt.Errorf("не указан secid %s-ноги", name)
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
// "qty":1,"with_futures":false,"call_secid":"...","put_secid":"...","futures_secid":"...",
// "hedge":{"rule":"hybrid",...},"time_stop_dte":14}
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
	spot, _ := getSpotPrice(req.Symbol)
	plan, err := buildShortStraddle(req.Symbol, req.Expiry, dteInDays(req.Expiry, time.Now()),
		spot, req.Strike, req.Qty, req.WithFutures, req.CallSecID, req.PutSecID, req.FuturesSecID,
		alorStraddlePricer)
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
		WithFutures: plan.WithFutures, NetCredit: plan.NetCredit, StopLevel: plan.StopLevel,
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

// GET /api/v1/straddles — open straddles with live position P&L.
func straddleListHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := []map[string]interface{}{}
	for _, s := range openStraddles() {
		item := map[string]interface{}{
			"id": s.ID, "symbol": s.Symbol, "expiry": s.Expiry,
			"strike": s.Strike, "qty": s.Qty, "with_futures": s.WithFutures,
			"net_credit": s.NetCredit, "stop_level": s.StopLevel,
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
	if pos, ok := quant.GetPositionByID(s.PositionID); ok {
		repricePosition(pos)
		if removed, found := quant.RemovePosition(pos.ID); found {
			quant.AddTrade(quant.Trade{
				ID: fmt.Sprintf("trd-%d", time.Now().Unix()), Strategy: "Short Straddle",
				Symbol: removed.Symbol, OpenedAt: removed.OpenedAt, ClosedAt: time.Now(),
				EntryValue: removed.EntryValue, ExitValue: removed.CurrentValue,
				RealizedPnL: removed.PnL, PnLPercent: removed.PnLPercent,
			})
		}
	}
	s.Status = "CLOSED"
	saveStraddleRecord(s)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
