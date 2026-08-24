package main

import (
	"encoding/json"
	"fmt"
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

// spreadTypes maps the vertical-spread type key to its display name and the
// leg specs relative to ATM (offset, isCall, isShort).
type spreadLegSpec struct {
	offset  int
	isCall  bool
	isShort bool
}

var spreadTypes = map[string]struct {
	Display string
	Debit   bool
	Specs   []spreadLegSpec
}{
	"bull_put": {
		Display: "Bull Put Spread",
		Specs:   []spreadLegSpec{{-1, false, true}, {-2, false, false}}, // Sell OTM put, buy further OTM put
	},
	"bear_call": {
		Display: "Bear Call Spread",
		Specs:   []spreadLegSpec{{1, true, true}, {2, true, false}}, // Sell OTM call, buy further OTM call
	},
	"bull_call": {
		Display: "Bull Call Spread",
		Debit:   true,
		Specs:   []spreadLegSpec{{0, true, false}, {1, true, true}}, // Buy ATM call, sell OTM call
	},
	"bear_put": {
		Display: "Bear Put Spread",
		Debit:   true,
		Specs:   []spreadLegSpec{{0, false, false}, {-1, false, true}}, // Buy ATM put, sell OTM put
	},
}

// spreadLeg is a planned/executed leg of a vertical spread.
type spreadLeg struct {
	SecID       string  `json:"secid"`
	Side        string  `json:"side"`
	Strike      float64 `json:"strike"`
	IsCall      bool    `json:"is_call"`
	Price       float64 `json:"price"`
	Delta       float64 `json:"delta"`
	Theta       float64 `json:"theta"`
	MarginShort float64 `json:"margin_short"`
}

// spreadPlan is the full economics of a vertical spread before opening.
type spreadPlan struct {
	Symbol        string      `json:"symbol"`
	Type          string      `json:"type"`
	DisplayName   string      `json:"display_name"`
	Expiry        string      `json:"expiry"`
	DaysToExp     int         `json:"days_to_exp"`
	Spot          float64     `json:"spot_price"`
	Qty           int         `json:"qty"`
	Legs          []spreadLeg `json:"legs"`
	ShortStrike   float64     `json:"short_strike"`
	LongStrike    float64     `json:"long_strike"`
	WingWidth     float64     `json:"width_step"`
	NetCredit     float64     `json:"net_credit"` // >0 credit spread, <0 debit spread
	MaxProfit     float64     `json:"max_profit"`
	MaxLoss       float64     `json:"max_loss"`
	MarginShort   float64     `json:"margin_short"`
	ThetaPerCtr   float64     `json:"theta_per_contract"`
	DeltaPerCtr   float64     `json:"delta_per_contract"`
	CentralStrike float64     `json:"central_strike"`
	Multiplier    float64     `json:"multiplier"`
	IsDebit       bool        `json:"is_debit"`
}

// spreadRecord is a persisted open vertical-spread position with its own id,
// linked to a quant.Position for portfolio/risk tracking.
type spreadRecord struct {
	ID           string  `json:"id"`
	PositionID   string  `json:"position_id"`
	Symbol       string  `json:"symbol"`
	Type         string  `json:"type"`
	DisplayName  string  `json:"display_name"`
	Expiry       string  `json:"expiry"`
	Qty          int     `json:"qty"`
	ShortStrike  float64 `json:"short_strike"`
	LongStrike   float64 `json:"long_strike"`
	WingWidth    float64 `json:"width_step"`
	NetCredit    float64 `json:"net_credit"`
	MaxProfit    float64 `json:"max_profit"`
	MaxLoss      float64 `json:"max_loss"`
	Margin       float64 `json:"margin"`
	OpenedAt     string  `json:"opened_at"`
	Status       string  `json:"status"` // OPEN / CLOSED / ROLLED
	RollCount    int     `json:"roll_count"`
	// Management rules — filled by user-provided spread rules (Phase 10+).
	StopLossPct    float64 `json:"stop_loss_pct"`
	TakeProfitPct  float64 `json:"take_profit_pct"`
	TrailingStopPct float64 `json:"trailing_stop_pct"`
	MaxHedgeDelta  float64 `json:"max_hedge_delta"`
	// Auto-management rules (spreads_manager.go).
	AutoRollDTE       int     `json:"auto_roll_dte"`
	RollCreditPct     float64 `json:"roll_credit_pct"`
	RollStrikeRiskPct float64 `json:"roll_strike_risk_pct"`
	AutoHedge         bool    `json:"auto_hedge"`
	Live              bool    `json:"live"`
	// Strategy state machine (vertical spread management spec, KNOWLEDGE.md §5).
	State           string  `json:"state"`                // VERTICAL / LADDER / RATIO / CONDOR / BACKSPREAD_LIKE
	EntrySpot       float64 `json:"entry_spot"`           // spot at open — reference for TPR sigma moves
	ProfitTargetPct float64 `json:"profit_target_pct"`    // T/P: share of max profit (spec 0.70–0.80)
	ProfitAction    string  `json:"profit_action"`        // CLOSE | ROLL | CONDOR at T/P
	TPRMode         string  `json:"tpr_mode"`             // OFF | ONE_DAY_SIGMA | MAX_LOSS
	TPRSigmaMult    float64 `json:"tpr_sigma_mult"`       // σ multiplier for ONE_DAY_SIGMA
	SigmaAnnual     float64 `json:"sigma_annual"`         // annualized vol used for the 1-day sigma
	AllowUndefined  bool    `json:"allow_undefined_risk"` // naked tails allowed (false by default, spec §22)
	ViewOverride    string  `json:"view_override"`        // BULLISH/SIDEWAYS/BEARISH reconstruction view at TPR
	TPR1            float64 `json:"tpr1"`                 // lower decision point (absolute spot)
	TPR2            float64 `json:"tpr2"`                 // upper decision point (absolute spot)
	RollAlpha       float64 `json:"roll_alpha"`           // share of realized profit risked on a profit roll
	CentralStrike   float64 `json:"central_strike"`       // ATM strike of the series at open
}

var (
	spreadsMu    sync.Mutex
	spreadStore  []spreadRecord
	spreadsFile  string
)

// initSpreads binds the spreads registry to the data directory and loads it.
func initSpreads(dataDir string) {
	spreadsMu.Lock()
	defer spreadsMu.Unlock()
	spreadsFile = filepath.Join(dataDir, "spreads.json")
	b, err := os.ReadFile(spreadsFile)
	if err == nil {
		_ = json.Unmarshal(b, &spreadStore)
	}
	if spreadStore == nil {
		spreadStore = []spreadRecord{}
	}
}

func persistSpreads() {
	spreadsMu.Lock()
	defer spreadsMu.Unlock()
	if spreadsFile == "" {
		return
	}
	b, _ := json.MarshalIndent(spreadStore, "", "  ")
	_ = os.WriteFile(spreadsFile, b, 0600)
}

// spreadRecordByID returns a copy of a spread record.
func spreadRecordByID(id string) (spreadRecord, bool) {
	spreadsMu.Lock()
	defer spreadsMu.Unlock()
	for _, s := range spreadStore {
		if s.ID == id {
			return s, true
		}
	}
	return spreadRecord{}, false
}

func saveSpreadRecord(s spreadRecord) {
	spreadsMu.Lock()
	defer spreadsMu.Unlock()
	for i := range spreadStore {
		if spreadStore[i].ID == s.ID {
			spreadStore[i] = s
			persistSpreadsLocked()
			return
		}
	}
	spreadStore = append(spreadStore, s)
	persistSpreadsLocked()
}

func persistSpreadsLocked() {
	if spreadsFile == "" {
		return
	}
	b, _ := json.MarshalIndent(spreadStore, "", "  ")
	_ = os.WriteFile(spreadsFile, b, 0600)
}

// openSpreads returns records with status OPEN.
func openSpreads() []spreadRecord {
	spreadsMu.Lock()
	defer spreadsMu.Unlock()
	out := make([]spreadRecord, 0, len(spreadStore))
	for _, s := range spreadStore {
		if s.Status == "OPEN" {
			out = append(out, s)
		}
	}
	return out
}

// optionChainFor returns the sorted unique strikes of the live option chain
// for symbol/expiry plus a strike/type lookup helper. Shared by the spread
// builder and the state-machine reconstructions.
func optionChainFor(symbol, expiry string) ([]float64, func(float64, bool) *optionContract, error) {
	if expiry == "" {
		if expiryT := currentSeriesExpiry(symbol); expiryT != nil {
			expiry = expiryT.Format("2006-01-02")
		}
	}
	chain := moexOptionsForAsset(symbol, expiry)
	if len(chain) == 0 {
		return nil, nil, fmt.Errorf("option chain not available for %s %s", symbol, expiry)
	}
	seen := map[float64]bool{}
	var strikes []float64
	for _, o := range chain {
		if !seen[o.Strike] {
			seen[o.Strike] = true
			strikes = append(strikes, o.Strike)
		}
	}
	sort.Float64s(strikes)
	find := func(strike float64, isCall bool) *optionContract {
		for i := range chain {
			if chain[i].Strike == strike && chain[i].IsCall == isCall {
				return &chain[i]
			}
		}
		return nil
	}
	return strikes, find, nil
}

// nearestStrikeFromStrikes picks the strike closest to spot from a sorted list.
func nearestStrikeFromStrikes(strikes []float64, spot float64) float64 {
	if len(strikes) == 0 {
		return spot
	}
	best := strikes[0]
	for _, s := range strikes {
		if math.Abs(s-spot) < math.Abs(best-spot) {
			best = s
		}
	}
	return best
}

// buildVerticalSpread computes the full plan of a vertical spread for the
// given symbol/type/expiry/qty without opening anything. If expiry is empty it
// uses the currently selected series.
func buildVerticalSpread(symbol, spreadType, expiry string, qty int) (*spreadPlan, error) {
	if symbol == "" {
		symbol = "Si"
	}
	meta, ok := spreadTypes[spreadType]
	if !ok {
		return nil, fmt.Errorf("unsupported spread type: %s", spreadType)
	}
	if qty <= 0 {
		qty = 1
	}

	spot, err := getSpotPrice(symbol)
	if err != nil || spot <= 0 {
		spot = 83200.0
	}

	if expiry == "" {
		expiryT := currentSeriesExpiry(symbol)
		if expiryT != nil {
			expiry = expiryT.Format("2006-01-02")
		}
	}

	strikes, findOpt, err := optionChainFor(symbol, expiry)
	if err != nil {
		return nil, err
	}

	atmStrike := nearestStrikeFromStrikes(strikes, spot)

	atmIdx := -1
	for i, s := range strikes {
		if s == atmStrike {
			atmIdx = i
			break
		}
	}
	if atmIdx < 0 {
		atmIdx = 0
	}

	days := dteInDays(expiry, time.Now())
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0
	rRate := 0.16

	plan := &spreadPlan{
		Symbol:      symbol,
		Type:        spreadType,
		DisplayName: meta.Display,
		Expiry:      expiry,
		DaysToExp:   dteInDays(expiry, time.Now()),
		Spot:        math.Round(spot*100) / 100,
		Qty:         qty,
		IsDebit:     meta.Debit,
		Legs:        []spreadLeg{},
	}

	var credit, debit, thetaTotal, deltaTotal, marginShort float64
	var shortStrike, longStrike float64

	for _, sp := range meta.Specs {
		idx := atmIdx + sp.offset
		if idx < 0 || idx >= len(strikes) {
			return nil, fmt.Errorf("%s legs out of chain range", meta.Display)
		}
		strike := strikes[idx]
		opt := findOpt(strike, sp.isCall)
		if opt == nil {
			return nil, fmt.Errorf("%s: option not found at %v", meta.Display, strike)
		}
		last, _, _, _ := moexOptionQuote(opt.SecID)
		if last <= 0 {
			last = opt.PrevPrice
		}
		iv := quant.ImpliedVolatility(sp.isCall, last, spot, strike, t, rRate)
		if iv <= 0 {
			iv = 0.30
		}
		g := quant.CalculateBlackScholes(sp.isCall, spot, strike, t, rRate, iv)

		side := "BUY"
		dir := 1.0
		if sp.isShort {
			side = "SELL"
			dir = -1.0
			credit += last
			shortStrike = strike
			if opt.IMNP > 0 {
				marginShort += opt.IMNP
			}
		} else {
			debit += last
			longStrike = strike
		}
		thetaTotal += dir * g.Theta
		deltaTotal += dir * g.Delta

		plan.Legs = append(plan.Legs, spreadLeg{
			SecID:       opt.SecID,
			Side:        side,
			Strike:      strike,
			IsCall:      sp.isCall,
			Price:       math.Round(last*100) / 100,
			Delta:       math.Round(dir*g.Delta*100) / 100,
			Theta:       math.Round(dir*g.Theta*100) / 100,
			MarginShort: opt.IMNP,
		})
	}

	// Wing width = |short - long| on the same side.
	wing := math.Abs(shortStrike - longStrike)
	if wing <= 0 {
		wing = 1.0
	}
	netCredit := credit - debit

	var maxProfit, maxLoss float64
	if meta.Debit {
		netDebit := debit - credit
		if netDebit < 0 {
			netDebit = 0
		}
		maxProfit = wing - netDebit
		maxLoss = netDebit
	} else {
		maxProfit = netCredit
		maxLoss = wing - netCredit
	}
	if maxProfit < 0 {
		maxProfit = 0
	}
	if maxLoss < 0 {
		maxLoss = 0
	}

	plan.ShortStrike = shortStrike
	plan.LongStrike = longStrike
	plan.WingWidth = math.Round(wing*10000) / 10000
	plan.NetCredit = math.Round(netCredit*100) / 100
	plan.MaxProfit = math.Round(maxProfit*100) / 100
	plan.MaxLoss = math.Round(maxLoss*100) / 100
	plan.MarginShort = math.Round(marginShort*100) / 100
	plan.ThetaPerCtr = math.Round(thetaTotal*100) / 100
	plan.DeltaPerCtr = math.Round(deltaTotal*100) / 100
	plan.CentralStrike = atmStrike
	plan.Multiplier = contractMultiplier(symbol)
	return plan, nil
}

// spreadPlanHandler returns the full economics of a vertical spread.
// URL: /api/v1/spreads/plan?symbol=Si&type=bull_put&qty=1&expiry=2026-09-17
func spreadPlanHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	spreadType := r.URL.Query().Get("type")
	expiry := r.URL.Query().Get("expiry")
	qty := 1
	if v := r.URL.Query().Get("qty"); v != "" {
		fmt.Sscanf(v, "%d", &qty)
	}
	plan, err := buildVerticalSpread(symbol, spreadType, expiry, qty)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(plan)
}

// spreadOpenHandler opens a vertical spread: creates the underlying
// quant.Position with two option legs and persists a spreadRecord.
// POST /api/v1/spreads/open {"symbol":"Si","type":"bull_put","expiry":"","qty":1,"live":false}
func spreadOpenHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Symbol string `json:"symbol"`
		Type   string `json:"type"`
		Expiry string `json:"expiry"`
		Qty    int    `json:"qty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Symbol == "" {
		req.Symbol = "Si"
	}

	plan, err := buildVerticalSpread(req.Symbol, req.Type, req.Expiry, req.Qty)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}

	mult := contractMultiplier(plan.Symbol)
	p := quant.Position{
		ID:       fmt.Sprintf("pos-%d", time.Now().UnixNano()/1e6),
		Strategy: plan.DisplayName,
		Symbol:   plan.Symbol,
		Expiry:   plan.Expiry,
		OpenedAt: time.Now(),
	}

	for _, l := range plan.Legs {
		entry := l.Price
		last := l.Price
		leg := quant.PositionLeg{
			SecID:        l.SecID,
			Symbol:       plan.Symbol,
			Kind:         "OPTION",
			Side:         l.Side,
			Quantity:     plan.Qty,
			Strike:       l.Strike,
			IsCall:       l.IsCall,
			EntryPrice:   entry,
			CurrentPrice: last,
		}
		p.Legs = append(p.Legs, leg)
		if l.Side == "SELL" {
			p.Margin += l.MarginShort * float64(plan.Qty)
		} else {
			p.Margin += plan.MaxLoss * mult * float64(plan.Qty)
		}
	}

	repricePosition(&p)
	quant.SavePosition(p)

	rec := spreadRecord{
		ID:           fmt.Sprintf("spr-%d", time.Now().UnixNano()/1e6),
		PositionID:   p.ID,
		Symbol:       plan.Symbol,
		Type:         plan.Type,
		DisplayName:  plan.DisplayName,
		Expiry:       plan.Expiry,
		Qty:          plan.Qty,
		ShortStrike:  plan.ShortStrike,
		LongStrike:   plan.LongStrike,
		WingWidth:    plan.WingWidth,
		NetCredit:    plan.NetCredit,
		MaxProfit:    plan.MaxProfit,
		MaxLoss:      plan.MaxLoss,
		Margin:       plan.MarginShort,
		OpenedAt:     time.Now().Format(time.RFC3339),
		Status:       "OPEN",
		RollCount:    0,
		TakeProfitPct: 0,
		TrailingStopPct: 0,
		MaxHedgeDelta: 0,
		// Knowledge-base defaults (KNOWLEDGE.md §1): stop at ~1.5x credit
		// (0.75 of max loss for a one-third-width credit), weekly MOEX series
		// roll on the last full week, profit capture target 50%, risk band 3%
		// around the short strike. All editable via the rules API/UI.
		StopLossPct:       0.75,
		AutoRollDTE:       defaultAutoRollDTE(dteInDays(plan.Expiry, time.Now())),
		RollCreditPct:     0.5,
		RollStrikeRiskPct: 0.03,
		// State machine defaults (KNOWLEDGE.md §5): T/P at 75% of max profit,
		// one-day-sigma TPR watch, undefined-risk reconstructions disabled.
		State:           "VERTICAL",
		EntrySpot:       plan.Spot,
		ProfitTargetPct: 0.75,
		ProfitAction:    "CLOSE",
		TPRMode:         "ONE_DAY_SIGMA",
		TPRSigmaMult:    1,
		SigmaAnnual:     0.30,
		RollAlpha:       1,
		CentralStrike:   plan.CentralStrike,
	}
	saveSpreadRecord(rec)

	if !spreadManagerEnabled() {
		startSpreadManager()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"spread":    rec,
		"position":  p,
		"portfolio": quant.GetPortfolio(),
	})
}

// spreadListHandler lists open spreads with live telemetry.
// URL: /api/v1/spreads
func spreadListHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	open := openSpreads()
	out := make([]map[string]interface{}, 0, len(open))
	for _, s := range open {
		item := map[string]interface{}{
			"id":           s.ID,
			"position_id":  s.PositionID,
			"symbol":       s.Symbol,
			"type":         s.Type,
			"display_name": s.DisplayName,
			"expiry":       s.Expiry,
			"qty":          s.Qty,
			"short_strike": s.ShortStrike,
			"long_strike":  s.LongStrike,
			"width_step":   s.WingWidth,
			"net_credit":   s.NetCredit,
			"max_profit":   s.MaxProfit,
			"max_loss":     s.MaxLoss,
			"margin":       s.Margin,
			"opened_at":    s.OpenedAt,
			"roll_count":   s.RollCount,
			"stop_loss_pct": s.StopLossPct,
			"take_profit_pct": s.TakeProfitPct,
			"trailing_stop_pct": s.TrailingStopPct,
			"max_hedge_delta": s.MaxHedgeDelta,
			"auto_roll_dte":   s.AutoRollDTE,
			"roll_credit_pct": s.RollCreditPct,
			"roll_strike_risk_pct": s.RollStrikeRiskPct,
			"auto_hedge":      s.AutoHedge,
			"live":            s.Live,
			"dte":             dteInDays(s.Expiry, time.Now()),
			"multiplier":      contractMultiplier(s.Symbol),
			"state":           s.State,
			"tpr1":            s.TPR1,
			"tpr2":            s.TPR2,
			"profit_target_pct": s.ProfitTargetPct,
			"profit_action":   s.ProfitAction,
			"tpr_mode":        s.TPRMode,
			"view_override":   s.ViewOverride,
			"central_strike":  s.CentralStrike,
		}

		// Live telemetry from the linked position.
		positions := quant.GetActivePositions()
		for i := range positions {
			if positions[i].ID == s.PositionID {
				p := &positions[i]
				repricePosition(p)
				quant.SavePosition(*p)
				item["entry_value"] = math.Round(p.EntryValue*100) / 100
				item["current_value"] = math.Round(p.CurrentValue*100) / 100
				item["pnl"] = math.Round(p.PnL*100) / 100
				item["pnl_percent"] = math.Round(p.PnLPercent*100) / 100
				item["net_delta"] = math.Round(p.Delta*100) / 100
				item["net_theta"] = math.Round(p.Theta*100) / 100
				item["hedge_legs"] = countHedgeLegs(p)

				// Per-leg profile: strike, live price and implied volatility
				// of every leg so the UI can show the full position breakdown.
				tYears := float64(dteInDays(s.Expiry, time.Now())) / 365.0
				if tYears <= 0 {
					tYears = 30.0 / 365.0
				}
				spot, _ := getSpotPrice(s.Symbol)
				if spot > 0 {
					item["spot"] = math.Round(spot*100) / 100
					if strikes, _, cerr := optionChainFor(s.Symbol, s.Expiry); cerr == nil && len(strikes) > 0 {
						item["atm_strike_now"] = nearestStrikeFromStrikes(strikes, spot)
					}
				}
				legs := make([]map[string]interface{}, 0, len(p.Legs))
				for _, l := range p.Legs {
					lm := map[string]interface{}{
						"secid":         l.SecID,
						"side":          l.Side,
						"kind":          l.Kind,
						"strike":        l.Strike,
						"is_call":       l.IsCall,
						"quantity":      l.Quantity,
						"entry_price":   math.Round(l.EntryPrice*100) / 100,
						"current_price": math.Round(l.CurrentPrice*100) / 100,
					}
					if l.Kind == "OPTION" && spot > 0 && l.CurrentPrice > 0 {
						if iv := quant.ImpliedVolatility(l.IsCall, l.CurrentPrice, spot, l.Strike, tYears, 0.16); iv > 0 {
							lm["iv_pct"] = math.Round(iv*1000) / 10
						}
					}
					// Quote freshness: source (mid/last/settle) and ISS row time.
					if q, ok := cachedOptionQuoteEx(l.SecID); ok {
						lm["quote_src"] = q.Src
						lm["quote_time"] = q.Updated
						lm["quote_stale"] = quoteIsStale(q.Updated, q.Src)
					}
					legs = append(legs, lm)
				}
				item["legs"] = legs
				break
			}
		}
		out = append(out, item)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"spreads": out,
		"note":    "Вертикальные спреды: Bull Put / Bear Call (кредитные), Bull Call / Bear Put (дебетовые).",
	})
}

// quoteIsStale reports whether the ISS marketdata row looks outdated: no
// update time at all, a settle-only mark, or an update more than 30 minutes
// behind the current time of day.
func quoteIsStale(updated, src string) bool {
	if src == "settle" || src == "none" || updated == "" {
		return true
	}
	t, err := time.Parse("15:04:05", updated)
	if err != nil {
		return true
	}
	now, _ := time.Parse("15:04:05", time.Now().Format("15:04:05"))
	return now.Sub(t) > 30*time.Minute || now.Sub(t) < -time.Hour
}

// isSpreadPositionID reports whether the position belongs to a managed
// vertical spread. Spread positions are shown on the Spreads tab only and are
// hidden from the central dashboard lists.
func isSpreadPositionID(id string) bool {
	spreadsMu.Lock()
	defer spreadsMu.Unlock()
	for _, s := range spreadStore {
		if s.PositionID == id {
			return true
		}
	}
	return false
}

// isSpreadTrade reports whether a closed trade originated from a vertical
// spread strategy (their display names all end with "Spread").
func isSpreadTrade(strategy string) bool {
	return strings.Contains(strategy, "Spread")
}

// countHedgeLegs returns how many FUTURES hedge legs the position has.
func countHedgeLegs(p *quant.Position) int {
	n := 0
	for _, l := range p.Legs {
		if l.Kind == "FUTURES" {
			n += l.Quantity
		}
	}
	return n
}

// spreadCloseHandler closes a spread: removes the linked position, records the
// trade and marks the spread CLOSED.
// POST /api/v1/spreads/close {"id":"spr-..."}
func spreadCloseHandler(w http.ResponseWriter, r *http.Request) {
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
	s, found := spreadRecordByID(req.ID)
	if !found || s.Status != "OPEN" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "spread not found or not open"})
		return
	}

	pos, found := quant.RemovePosition(s.PositionID)
	if !found {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "linked position not found"})
		return
	}
	repricePosition(&pos)
	trade := quant.Trade{
		ID:          fmt.Sprintf("trd-%d", time.Now().Unix()),
		Strategy:    pos.Strategy,
		Symbol:      pos.Symbol,
		OpenedAt:    pos.OpenedAt,
		ClosedAt:    time.Now(),
		EntryValue:  pos.EntryValue,
		ExitValue:   pos.CurrentValue,
		RealizedPnL: pos.PnL,
		PnLPercent:  pos.PnLPercent,
	}
	quant.AddTrade(trade)

	s.Status = "CLOSED"
	saveSpreadRecord(s)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"spread":    s,
		"trade":     trade,
		"portfolio": quant.GetPortfolio(),
		"stats":     quant.ComputeStats(),
	})
}

// spreadRollHandler rolls a spread to the next futures series: closes the
// current position, opens a new one in the next series and bumps RollCount.
// POST /api/v1/spreads/roll {"id":"spr-...","live":false}
func spreadRollHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID   string `json:"id"`
		Live bool   `json:"live"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	s, found := spreadRecordByID(req.ID)
	if !found || s.Status != "OPEN" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "spread not found or not open"})
		return
	}

	roll := nextRollSeries(s.Symbol, s.Expiry)
	if roll.NextSeries == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "no next series available"})
		return
	}

	// 1) Close the current position and record the trade.
	pos, found := quant.RemovePosition(s.PositionID)
	if !found {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "linked position not found"})
		return
	}
	repricePosition(&pos)
	quant.AddTrade(quant.Trade{
		ID:          fmt.Sprintf("trd-%d", time.Now().Unix()),
		Strategy:    pos.Strategy,
		Symbol:      pos.Symbol,
		OpenedAt:    pos.OpenedAt,
		ClosedAt:    time.Now(),
		EntryValue:  pos.EntryValue,
		ExitValue:   pos.CurrentValue,
		RealizedPnL: pos.PnL,
		PnLPercent:  pos.PnLPercent,
	})
	s.Status = "ROLLED"
	saveSpreadRecord(s)

	// 2) Open the same spread in the next series.
	plan, err := buildVerticalSpread(s.Symbol, s.Type, roll.NextExpiry, s.Qty)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "roll build failed: " + err.Error()})
		return
	}

	mult := contractMultiplier(plan.Symbol)
	p := quant.Position{
		ID:       fmt.Sprintf("pos-%d", time.Now().UnixNano()/1e6),
		Strategy: plan.DisplayName,
		Symbol:   plan.Symbol,
		Expiry:   plan.Expiry,
		OpenedAt: time.Now(),
	}
	for _, l := range plan.Legs {
		leg := quant.PositionLeg{
			SecID:        l.SecID,
			Symbol:       plan.Symbol,
			Kind:         "OPTION",
			Side:         l.Side,
			Quantity:     plan.Qty,
			Strike:       l.Strike,
			IsCall:       l.IsCall,
			EntryPrice:   l.Price,
			CurrentPrice: l.Price,
		}
		p.Legs = append(p.Legs, leg)
		if l.Side == "SELL" {
			p.Margin += l.MarginShort * float64(plan.Qty)
		} else {
			p.Margin += plan.MaxLoss * mult * float64(plan.Qty)
		}
	}
	repricePosition(&p)
	quant.SavePosition(p)

	newRec := spreadRecord{
		ID:            fmt.Sprintf("spr-%d", time.Now().UnixNano()/1e6),
		PositionID:    p.ID,
		Symbol:        plan.Symbol,
		Type:          plan.Type,
		DisplayName:   plan.DisplayName,
		Expiry:        plan.Expiry,
		Qty:           plan.Qty,
		ShortStrike:   plan.ShortStrike,
		LongStrike:    plan.LongStrike,
		WingWidth:     plan.WingWidth,
		NetCredit:     plan.NetCredit,
		MaxProfit:     plan.MaxProfit,
		MaxLoss:       plan.MaxLoss,
		Margin:        plan.MarginShort,
		OpenedAt:      time.Now().Format(time.RFC3339),
		Status:        "OPEN",
		RollCount:     s.RollCount + 1,
		StopLossPct:   s.StopLossPct,
		TakeProfitPct: s.TakeProfitPct,
		TrailingStopPct: s.TrailingStopPct,
		MaxHedgeDelta: s.MaxHedgeDelta,
	}
	saveSpreadRecord(newRec)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"message":       fmt.Sprintf("Ролл выполнен: %s → %s (%s)", s.Expiry, roll.NextExpiry, roll.NextSeries),
		"previous":      s,
		"spread":        newRec,
		"position":      p,
		"next_series":   roll.NextSeries,
		"portfolio":     quant.GetPortfolio(),
		"stats":         quant.ComputeStats(),
	})
}

// spreadHedgeHandler delta-hedges a spread using its linked position.
// POST /api/v1/spreads/hedge {"id":"spr-...","live":false}
func spreadHedgeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID   string `json:"id"`
		Live bool   `json:"live"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	s, found := spreadRecordByID(req.ID)
	if !found || s.Status != "OPEN" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "spread not found or not open"})
		return
	}
	hedgePositionByID(s.PositionID, req.Live, w)
}

// hedgePositionByID delta-hedges the position with the given id using a
// futures leg. Shared by /api/v1/positions/hedge and /api/v1/spreads/hedge.
func hedgePositionByID(id string, live bool, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")

	positions := quant.GetActivePositions()
	var pos *quant.Position
	for i := range positions {
		if positions[i].ID == id {
			pos = &positions[i]
			break
		}
	}
	if pos == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "position not found"})
		return
	}

	repricePosition(pos)

	// Net delta after repricing; futures hedge offsets it to ~0.
	netDelta := pos.Delta
	hedgeQty := int(math.Round(-netDelta))
	if hedgeQty == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Позиция уже дельта-нейтральна (net delta ≈ 0)",
			"delta":   netDelta,
			"hedge":   0,
		})
		return
	}
	side := "buy"
	if hedgeQty < 0 {
		side = "sell"
		hedgeQty = -hedgeQty
	}
	sideUp := "BUY"
	if side == "sell" {
		sideUp = "SELL"
	}

	futureSecid := selectedSeries[pos.Symbol]
	if futureSecid == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "no futures series for " + pos.Symbol})
		return
	}
	// For premium equity options (SBER/SBERP) the hedge instrument is the
	// underlying share itself (Alor symbol = ticker), not a futures code.
	if _, isEquity := equityOptions[pos.Symbol]; isEquity {
		futureSecid = pos.Symbol
	}

	// Place a real order when requested and Alor is configured.
	orderNote := "бумажный хедж (Alor не подключён)"
	if live && alorExec != nil {
		resp, err := alorExec.DeltaHedge(futureSecid, netDelta)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Alor hedge failed: " + err.Error(),
			})
			return
		}
		if resp != nil && resp.Message != "" {
			orderNote = resp.Message
		} else {
			orderNote = "ордер отправлен в Alor"
		}
	}

	// Record the hedge leg on the position (paper tracking in both cases).
	spot, _ := getSpotPrice(pos.Symbol)
	if spot <= 0 {
		spot = pos.CurrentValue
	}
	mult := contractMultiplier(pos.Symbol)
	margin := moexFutureInitialMargin(futureSecid)
	if margin <= 0 {
		margin = spot * mult * 0.15
	}

	hedgeLeg := quant.PositionLeg{
		SecID:        futureSecid,
		Symbol:       pos.Symbol,
		Kind:         "FUTURES",
		Side:         sideUp,
		Quantity:     hedgeQty,
		EntryPrice:   spot,
		CurrentPrice: spot,
	}
	pos.Legs = append(pos.Legs, hedgeLeg)
	pos.Margin += margin * float64(hedgeQty)
	repricePosition(pos)
	quant.SavePosition(*pos)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"message":   fmt.Sprintf("Хедж: %s %d контрактов %s (%s)", sideUp, hedgeQty, futureSecid, orderNote),
		"side":      sideUp,
		"hedge_qty": hedgeQty,
		"delta":     pos.Delta,
		"position":  pos,
		"portfolio": quant.GetPortfolio(),
		"stats":     quant.ComputeStats(),
	})
}

// rollSpreadMsg is a helper that builds a human-readable roll message.
func rollSpreadMsg(s spreadRecord) string {
	return fmt.Sprintf("%s %s: %s → %s", s.DisplayName, s.Symbol, s.Expiry, s.Expiry)
}

var _ = strings.TrimSpace // keep strings import (used by future rules module)