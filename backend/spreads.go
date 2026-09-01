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
	ID          string  `json:"id"`
	PositionID  string  `json:"position_id"`
	Symbol      string  `json:"symbol"`
	Type        string  `json:"type"`
	DisplayName string  `json:"display_name"`
	Expiry      string  `json:"expiry"`
	Qty         int     `json:"qty"`
	ShortStrike float64 `json:"short_strike"`
	LongStrike  float64 `json:"long_strike"`
	WingWidth   float64 `json:"width_step"`
	NetCredit   float64 `json:"net_credit"`
	MaxProfit   float64 `json:"max_profit"`
	MaxLoss     float64 `json:"max_loss"`
	Margin      float64 `json:"margin"`
	OpenedAt    string  `json:"opened_at"`
	Status      string  `json:"status"` // OPEN / CLOSED / ROLLED
	RollCount   int     `json:"roll_count"`
	// Management rules — filled by user-provided spread rules (Phase 10+).
	StopLossPct     float64 `json:"stop_loss_pct"`
	TakeProfitPct   float64 `json:"take_profit_pct"`
	TrailingStopPct float64 `json:"trailing_stop_pct"`
	MaxHedgeDelta   float64 `json:"max_hedge_delta"`
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
	spreadsMu   sync.Mutex
	spreadStore []spreadRecord
	spreadsFile string
)

// initSpreads binds the spreads registry to the data directory and loads it.
func initSpreads(dataDir string) {
	spreadsMu.Lock()
	defer spreadsMu.Unlock()
	spreadsFile = filepath.Join(dataDir, "spreads.json")
	b, err := os.ReadFile(spreadsFile)
	if err == nil {
		if uerr := json.Unmarshal(b, &spreadStore); uerr != nil {
			// Never wipe a corrupt registry — keep a recovery copy.
			_ = os.Rename(spreadsFile, fmt.Sprintf("%s.broken-%d", spreadsFile, time.Now().Unix()))
			spreadStore = []spreadRecord{}
		}
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
	b, err := json.MarshalIndent(spreadStore, "", "  ")
	if err != nil {
		return
	}
	tmp := spreadsFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, spreadsFile)
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
		if last <= 0 {
			// Opening or rolling into a leg with no market price would record
			// entry=0 and corrupt the P&L — refuse instead.
			return nil, fmt.Errorf("нет рыночной цены для %s — операция отменена", opt.SecID)
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
	plan.NetCredit = math.Round(netCredit*float64(qty)*100) / 100
	plan.MaxProfit = math.Round(maxProfit*float64(qty)*100) / 100
	plan.MaxLoss = math.Round(maxLoss*float64(qty)*100) / 100
	plan.MarginShort = math.Round(marginShort*float64(qty)*100) / 100
	plan.ThetaPerCtr = math.Round(thetaTotal*100) / 100
	plan.DeltaPerCtr = math.Round(deltaTotal*100) / 100
	plan.CentralStrike = atmStrike
	plan.Multiplier = contractMultiplier(symbol)
	return plan, nil
}

// rollLegSpec describes a leg of the resulting position after a roll. A
// "kept" leg stays at its current series/strike; a "rolled" leg moves to a
// new series and (optionally) a new strike.
type rollLegSpec struct {
	Side          string // "BUY" | "SELL"
	IsCall        bool
	CurrentStrike float64 // strike in the current/kept series
	// For a rolled leg in a target series:
	Roll         bool // true → move to TargetStrike; false → keep CurrentStrike
	TargetStrike float64
	SecID        string // effective secid after resolution (may be set by caller)
}

// buildSpreadFromLegs computes the economics of a spread made from explicit
// option legs (side/strike/call) in a given expiry, reusing the same quote and
// Black-Scholes machinery as buildVerticalSpread. It supports zero or more
// SELL legs and one or more BUY legs (verticals, and 1-legged rolls). It is
// the engine for the roll preview and the per-leg roll execution.
//
// typeName/displayName keep the report human-readable; they are informational.
func buildSpreadFromLegs(symbol, expiry string, qty int, legs []rollLegSpec, isDebit bool, typeName, displayName string) (*spreadPlan, error) {
	if symbol == "" {
		symbol = "Si"
	}
	if expiry == "" {
		if expiryT := currentSeriesExpiry(symbol); expiryT != nil {
			expiry = expiryT.Format("2006-01-02")
		}
	}
	if qty <= 0 {
		qty = 1
	}

	spot, err := getSpotPrice(symbol)
	if err != nil || spot <= 0 {
		spot = 83200.0
	}

	strikes, findOpt, err := optionChainFor(symbol, expiry)
	if err != nil {
		return nil, err
	}
	_ = strikes // used only via findOpt for secid/GO lookup

	days := dteInDays(expiry, time.Now())
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0
	rRate := 0.16

	plan := &spreadPlan{
		Symbol:      symbol,
		Type:        typeName,
		DisplayName: displayName,
		Expiry:      expiry,
		DaysToExp:   dteInDays(expiry, time.Now()),
		Spot:        math.Round(spot*100) / 100,
		Qty:         qty,
		IsDebit:     isDebit,
		Legs:        []spreadLeg{},
	}

	var credit, debit, thetaTotal, deltaTotal, marginShort float64
	var shortStrike, longStrike float64

	for _, sp := range legs {
		side := sp.Side
		if side == "" {
			side = "BUY"
		}
		dir := 1.0
		if side == "SELL" {
			dir = -1.0
		}
		opt := findOpt(sp.TargetStrike, sp.IsCall)
		if opt == nil {
			return nil, fmt.Errorf("option not found at %v", sp.TargetStrike)
		}
		last, bid, offer, _ := moexOptionQuote(opt.SecID)
		if last <= 0 {
			last = opt.PrevPrice
		}
		_ = bid
		_ = offer
		if last <= 0 {
			return nil, fmt.Errorf("нет рыночной цены для %s — операция отменена", opt.SecID)
		}
		iv := quant.ImpliedVolatility(sp.IsCall, last, spot, sp.TargetStrike, t, rRate)
		if iv <= 0 {
			iv = 0.30
		}
		g := quant.CalculateBlackScholes(sp.IsCall, spot, sp.TargetStrike, t, rRate, iv)

		if side == "SELL" {
			credit += last
			shortStrike = sp.TargetStrike
			if opt.IMNP > 0 {
				marginShort += opt.IMNP
			}
		} else {
			debit += last
			longStrike = sp.TargetStrike
		}
		thetaTotal += dir * g.Theta
		deltaTotal += dir * g.Delta

		plan.Legs = append(plan.Legs, spreadLeg{
			SecID:       opt.SecID,
			Side:        side,
			Strike:      sp.TargetStrike,
			IsCall:      sp.IsCall,
			Price:       math.Round(last*100) / 100,
			Delta:       math.Round(dir*g.Delta*100) / 100,
			Theta:       math.Round(dir*g.Theta*100) / 100,
			MarginShort: opt.IMNP,
		})
	}

	wing := math.Abs(shortStrike - longStrike)
	if wing <= 0 {
		wing = 1.0
	}
	netCredit := credit - debit

	var maxProfit, maxLoss float64
	if isDebit {
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

	atmStrike := nearestStrikeFromStrikes(strikes, spot)

	plan.ShortStrike = shortStrike
	plan.LongStrike = longStrike
	plan.WingWidth = math.Round(wing*10000) / 10000
	plan.NetCredit = math.Round(netCredit*float64(qty)*100) / 100
	plan.MaxProfit = math.Round(maxProfit*float64(qty)*100) / 100
	plan.MaxLoss = math.Round(maxLoss*float64(qty)*100) / 100
	plan.MarginShort = math.Round(marginShort*float64(qty)*100) / 100
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
		ID:              fmt.Sprintf("spr-%d", time.Now().UnixNano()/1e6),
		PositionID:      p.ID,
		Symbol:          plan.Symbol,
		Type:            plan.Type,
		DisplayName:     plan.DisplayName,
		Expiry:          plan.Expiry,
		Qty:             plan.Qty,
		ShortStrike:     plan.ShortStrike,
		LongStrike:      plan.LongStrike,
		WingWidth:       plan.WingWidth,
		NetCredit:       plan.NetCredit,
		MaxProfit:       plan.MaxProfit,
		MaxLoss:         plan.MaxLoss,
		Margin:          plan.MarginShort,
		OpenedAt:        time.Now().Format(time.RFC3339),
		Status:          "OPEN",
		RollCount:       0,
		TakeProfitPct:   0,
		TrailingStopPct: 0,
		MaxHedgeDelta:   0,
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
			"id":                   s.ID,
			"position_id":          s.PositionID,
			"symbol":               s.Symbol,
			"type":                 s.Type,
			"display_name":         s.DisplayName,
			"expiry":               s.Expiry,
			"qty":                  s.Qty,
			"short_strike":         s.ShortStrike,
			"long_strike":          s.LongStrike,
			"width_step":           s.WingWidth,
			"net_credit":           s.NetCredit,
			"max_profit":           s.MaxProfit,
			"max_loss":             s.MaxLoss,
			"margin":               s.Margin,
			"opened_at":            s.OpenedAt,
			"roll_count":           s.RollCount,
			"stop_loss_pct":        s.StopLossPct,
			"take_profit_pct":      s.TakeProfitPct,
			"trailing_stop_pct":    s.TrailingStopPct,
			"max_hedge_delta":      s.MaxHedgeDelta,
			"auto_roll_dte":        s.AutoRollDTE,
			"roll_credit_pct":      s.RollCreditPct,
			"roll_strike_risk_pct": s.RollStrikeRiskPct,
			"auto_hedge":           s.AutoHedge,
			"live":                 s.Live,
			"dte":                  dteInDays(s.Expiry, time.Now()),
			"multiplier":           contractMultiplier(s.Symbol),
			"state":                s.State,
			"tpr1":                 s.TPR1,
			"tpr2":                 s.TPR2,
			"profit_target_pct":    s.ProfitTargetPct,
			"profit_action":        s.ProfitAction,
			"tpr_mode":             s.TPRMode,
			"view_override":        s.ViewOverride,
			"central_strike":       s.CentralStrike,
		}

		// Live telemetry from the linked position.
		positions := quant.GetActivePositions()
		foundPos := false
		for i := range positions {
			if positions[i].ID == s.PositionID {
				foundPos = true
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
						"entry_zero":    l.Kind == "OPTION" && l.EntryPrice <= 0,
					}
					if l.Kind == "OPTION" {
						// Quote freshness: source of the live two-sided book
						// (if any) plus the provenance of the final mark.
						if q, ok := cachedOptionQuoteEx(l.SecID); ok {
							lm["quote_src"] = q.Src
							lm["quote_time"] = q.Updated
							lm["quote_stale"] = quoteIsStale(q.Updated, q.Src)
						}
						if spot > 0 {
							q, qOk := cachedOptionQuoteEx(l.SecID)
							if qOk && l.CurrentPrice > 0 {
								msrc := quoteMarkSrc(q, l.CurrentPrice)
								lm["mark_src"] = msrc
							}
							if l.CurrentPrice > 0 {
								if iv := quant.ImpliedVolatility(l.IsCall, l.CurrentPrice, spot, l.Strike, tYears, 0.16); iv > 0 {
									lm["iv_pct"] = math.Round(iv*1000) / 10
								}
							}
						}
					}
					legs = append(legs, lm)
				}
				item["legs"] = legs

				// Theoretical PnL: BS fair value of the spread at current
				// spot / IV / DTE so the UI can show how PnL responds to
				// volatility and time decay.
				if spot > 0 {
					mult := contractMultiplier(s.Symbol)
					th := spreadTheoLive(p.Legs, spot, s.Expiry, mult)
					theoPnL := th.Value - p.EntryValue
					item["theo_value"] = math.Round(th.Value*100) / 100
					item["theo_pnl"] = math.Round(theoPnL*100) / 100
					item["iv_sensitivity"] = math.Round(th.Vega*100) / 100 // ₽ per 1% IV
					item["daily_decay"] = math.Round(p.Theta*100) / 100    // ₽ per day
					item["gamma_total"] = math.Round(th.Gamma*10000) / 10000
				}
				break
			}
		}
		item["position_found"] = foundPos
		// Dynamic stop / take‑profit (populated later from core candidate; here a placeholder).
		item["stop_loss"] = 0
		item["take_profit"] = 0
		out = append(out, item)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"spreads": out,
		"note":    "Вертикальные спреды: Bull Put / Bear Call (кредитные), Bull Call / Bear Put (дебетовые).",
	})
}

// theoSpread holds the Black-Scholes fair value and sensitivity aggregate of
// a spread at the current spot / IV / DTE.
type theoSpread struct {
	Value float64 // fair value in ₽ (mult × qty applied per leg)
	Vega  float64 // PnL change per 1% IV move, ₽
	Gamma float64 // gamma per underlying unit, ₽ per point
}

// spreadTheoLive reprices the spread legs with Black-Scholes using each
// option's own market-implied IV, so the fair value mirrors the market quote
// (units: rubles, sign × multiplier × quantity per leg — same as repricePosition).
func spreadTheoLive(legs []quant.PositionLeg, spot float64, expiry string, mult float64) theoSpread {
	rRate := 0.16
	tYears := float64(dteInDays(expiry, time.Now())) / 365.0
	if tYears <= 0 {
		tYears = 30.0 / 365.0
	}
	var th theoSpread
	for _, l := range legs {
		q := float64(l.Quantity)
		dir := 1.0
		if l.Side == "SELL" {
			dir = -1
		}
		if l.Kind != "OPTION" {
			th.Value += dir * l.CurrentPrice * mult * q
			continue
		}
		iv := 0.30
		if l.CurrentPrice > 0 {
			if ivBack := quant.ImpliedVolatility(l.IsCall, l.CurrentPrice, spot, l.Strike, tYears, rRate); ivBack > 0 {
				iv = ivBack
			}
		}
		g := quant.CalculateBlackScholes(l.IsCall, spot, l.Strike, tYears, rRate, iv)
		th.Value += dir * g.Price * mult * q
		th.Vega += dir * g.Vega * mult * q // g.Vega already per 1% IV
		th.Gamma += dir * g.Gamma * mult * q
	}
	return th
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
		// Orphaned spread (position storage was reset or wiped): there is
		// nothing to settle — just mark the record closed so the UI unsticks.
		s.Status = "CLOSED"
		saveSpreadRecord(s)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"spread":  s,
			"note":    "позиция не найдена в хранилище — спред помечен закрытым без записи сделки.",
		})
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
	enrichTradeContext(&trade, s.Symbol, s.Expiry, s.EntrySpot)
	quant.AddTrade(trade)

	s.Status = "CLOSED"
	saveSpreadRecord(s)

	notifyStructureClosed(&s, "Закрыто вручную", trade.RealizedPnL, trade.PnLPercent)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"spread":    s,
		"trade":     trade,
		"portfolio": quant.GetPortfolio(),
		"stats":     quant.ComputeStats(),
	})
}

// rollRequestLeg carries a per-leg override supplied by the caller when
// rolling: which leg (side/call) to move and to which new strike.
type rollRequestLeg struct {
	Side   string  `json:"side"`
	IsCall bool    `json:"is_call"`
	Strike float64 `json:"strike"`
	Roll   bool    `json:"roll"`
}

// spreadRollHandler rolls a spread into a chosen series, optionally moving one
// or both legs to new strikes. It closes the current position, opens the new
// one and bumps RollCount.
// POST /api/v1/spreads/roll
//
//	{
//	  "id":"spr-...",
//	  "live":false,
//	  "series":"2026-10-15",          // optional target expiry (auto if empty)
//	  "legs":[                        // optional per-leg overrides (by side)
//	     {"side":"SELL","is_call":true,"strike":81000,"roll":true}
//	  ]
//	}
func spreadRollHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID     string           `json:"id"`
		Live   bool             `json:"live"`
		Series string           `json:"series"`
		Legs   []rollRequestLeg `json:"legs"`
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

	// Resolve the target series (default: earliest next).
	targetExpiry := req.Series
	var nextSeriesCode string
	if targetExpiry == "" {
		roll := nextRollSeries(s.Symbol, s.Expiry)
		if roll.NextExpiry == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "no next series available"})
			return
		}
		targetExpiry = roll.NextExpiry
		nextSeriesCode = roll.NextSeries
	}

	// 1) Close the current position and record the trade.
	pos, found := quant.RemovePosition(s.PositionID)
	if !found {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "linked position not found"})
		return
	}
	repricePosition(&pos)
	rollTrade := quant.Trade{
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
	enrichTradeContext(&rollTrade, s.Symbol, s.Expiry, s.EntrySpot)
	quant.AddTrade(rollTrade)
	s.Status = "ROLLED"
	saveSpreadRecord(s)

	// 2) Build the resulting legs. Every option leg moves to the target series.
	// If the caller chose a new strike for it, use that; otherwise keep the
	// current strike (valued in the new series).
	specs := make([]rollLegSpec, 0, 2)
	for _, l := range pos.Legs {
		if l.Kind != "OPTION" {
			continue
		}
		strike := l.Strike
		for _, over := range req.Legs {
			if over.Side == l.Side && over.IsCall == l.IsCall && over.Strike > 0 {
				strike = over.Strike
				break
			}
		}
		specs = append(specs, rollLegSpec{
			Side:          l.Side,
			IsCall:        l.IsCall,
			CurrentStrike: l.Strike,
			TargetStrike:  strike,
		})
	}

	plan, err := buildSpreadFromLegs(s.Symbol, targetExpiry, s.Qty, specs, metaDebitForType(s.Type), s.Type, s.DisplayName)
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
		ID:              fmt.Sprintf("spr-%d", time.Now().UnixNano()/1e6),
		PositionID:      p.ID,
		Symbol:          plan.Symbol,
		Type:            plan.Type,
		DisplayName:     plan.DisplayName,
		Expiry:          plan.Expiry,
		Qty:             plan.Qty,
		ShortStrike:     plan.ShortStrike,
		LongStrike:      plan.LongStrike,
		WingWidth:       plan.WingWidth,
		NetCredit:       plan.NetCredit,
		MaxProfit:       plan.MaxProfit,
		MaxLoss:         plan.MaxLoss,
		Margin:          plan.MarginShort,
		OpenedAt:        time.Now().Format(time.RFC3339),
		Status:          "OPEN",
		RollCount:       s.RollCount + 1,
		StopLossPct:     s.StopLossPct,
		TakeProfitPct:   s.TakeProfitPct,
		TrailingStopPct: s.TrailingStopPct,
		MaxHedgeDelta:   s.MaxHedgeDelta,
	}
	saveSpreadRecord(newRec)

	nextDisplay := targetExpiry
	if nextSeriesCode != "" {
		nextDisplay = fmt.Sprintf("%s (%s)", targetExpiry, nextSeriesCode)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"message":     fmt.Sprintf("Ролл выполнен: %s → %s", s.Expiry, nextDisplay),
		"previous":    s,
		"spread":      newRec,
		"position":    p,
		"next_series": nextSeriesCode,
		"portfolio":   quant.GetPortfolio(),
		"stats":       quant.ComputeStats(),
	})
}

// metaDebitForType returns whether a spread type is a debit structure.
func metaDebitForType(t string) bool {
	m, ok := spreadTypes[t]
	if !ok {
		return false
	}
	return m.Debit
}

// rollChainItem describes one option available to open as a rolled leg in the
// target series, with full liquidity (top-of-book bid/ask + best-effort depth).
type rollChainItem struct {
	SecID     string           `json:"secid"`
	Strike    float64          `json:"strike"`
	IsCall    bool             `json:"is_call"`
	Side      string           `json:"side"` // suggested side for building the structure
	Mid       float64          `json:"mid"`
	Bid       float64          `json:"bid"`
	Ask       float64          `json:"ask"`
	SpreadPct float64          `json:"spread_pct"` // (ask-bid)/mid %
	Depth     []orderBookLevel `json:"depth"`      // Alor depth, empty if unavailable
	DepthSrc  string           `json:"depth_src"`  // "alor" | "iss"
}

type orderBookLevel struct {
	Price  float64 `json:"price"`
	Volume int     `json:"volume"`
	Side   string  `json:"side"` // "bid" | "ask"
}

// spreadRollPreviewHandler returns everything needed to preview and execute a
// roll: the current legs with close liquidity, the list of available target
// series, and — for a chosen series — the per-leg strike/quote menu with
// economics. Re-run it with different series/strike params as the user picks.
//
// GET /api/v1/spreads/roll/preview?id=spr-...&series=2026-10-15
//
//	&leg=SELL&leg=BUY&strike=81000&strike=82500
func spreadRollPreviewHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := r.URL.Query().Get("id")
	s, found := spreadRecordByID(id)
	if !found || s.Status != "OPEN" {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "spread not found or not open"})
		return
	}

	// Current position legs with close liquidity.
	positions := quant.GetActivePositions()
	var pos *quant.Position
	for i := range positions {
		if positions[i].ID == s.PositionID {
			pos = &positions[i]
			break
		}
	}
	current := []map[string]interface{}{}
	closeValue := 0.0
	mult := contractMultiplier(s.Symbol)
	if pos != nil {
		repricePosition(pos)
		quant.SavePosition(*pos)
		for _, l := range pos.Legs {
			q := optionQuoteForDepth(l.SecID)
			ml := map[string]interface{}{
				"secid": l.SecID, "side": l.Side, "kind": l.Kind,
				"strike": l.Strike, "is_call": l.IsCall,
				"entry_price":   math.Round(l.EntryPrice*100) / 100,
				"current_price": math.Round(l.CurrentPrice*100) / 100,
				"bid":           q.Bid, "ask": q.Offer, "mid": q.Price,
			}
			current = append(current, ml)
			// Close = sell what we own, buy what we sold (realizing at mid).
			dir := 1.0
			if l.Side == "SELL" {
				dir = -1
			}
			if l.Kind == "OPTION" {
				closeValue += dir * q.Price * mult * float64(l.Quantity)
			}
		}
	}

	// Available target series (newest first) → earliest-after-current first.
	seriesList := []map[string]interface{}{}
	for _, c := range optionSeriesForSymbol(s.Symbol) {
		if c.LastDelDate <= time.Now().Format("2006-01-02") {
			continue
		}
		seriesList = append(seriesList, map[string]interface{}{
			"code": c.Code, "short_name": c.ShortName, "expiry": c.LastDelDate,
			"dtc": dteInDays(c.LastDelDate, time.Now()),
		})
	}

	// Resolve active target series.
	targetExpiry := r.URL.Query().Get("series")
	if targetExpiry == "" && len(seriesList) > 0 {
		nr := nextRollSeries(s.Symbol, s.Expiry)
		targetExpiry = nr.NextExpiry
	}
	if targetExpiry == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "no target series", "series_options": seriesList})
		return
	}

	// Per-leg override strikes supplied via repeated leg/strike query params.
	legSides := r.URL.Query()["leg"]
	legStrikes := r.URL.Query()["strike"]
	overrides := map[string]float64{}
	for i, ls := range legSides {
		if i < len(legStrikes) {
			var st float64
			fmt.Sscanf(legStrikes[i], "%f", &st)
			overrides[strings.ToUpper(ls)] = st
		}
	}

	// Build the strike/quote menu for each leg's side, and the resulting plan.
	chainItems := []map[string]interface{}{}
	specs := []rollLegSpec{}
	underlyingOrderbook := []orderBookLevel{}
	underlyingDepthSrc := ""
	if pos != nil {
		if code, ok := futuresSeriesAlor(s.Symbol); ok && alorMarket != nil {
			if ob, err := alorMarket.FetchOrderbook("MOEX", code); err == nil {
				for _, b := range ob.Bids {
					underlyingOrderbook = append(underlyingOrderbook, orderBookLevel{Price: b.Price, Volume: b.Volume, Side: "bid"})
				}
				for _, a := range ob.Asks {
					underlyingOrderbook = append(underlyingOrderbook, orderBookLevel{Price: a.Price, Volume: a.Volume, Side: "ask"})
				}
				underlyingDepthSrc = "alor"
			}
		}
		for _, l := range pos.Legs {
			if l.Kind != "OPTION" {
				continue
			}
			side := l.Side
			strike := l.Strike
			if st, ok := overrides[strings.ToUpper(side)]; ok && st > 0 {
				strike = st
			}
			items := optionChainMenu(s.Symbol, targetExpiry, side, l.IsCall, l.Strike)
			chainItems = append(chainItems, map[string]interface{}{
				"side": side, "is_call": l.IsCall, "current_strike": l.Strike,
				"options": items,
			})
			specs = append(specs, rollLegSpec{
				Side: l.Side, IsCall: l.IsCall, CurrentStrike: l.Strike,
				TargetStrike: strike,
			})
		}
	}

	plan, err := buildSpreadFromLegs(s.Symbol, targetExpiry, s.Qty, specs, metaDebitForType(s.Type), s.Type, s.DisplayName)
	var planErr string
	if err != nil {
		planErr = err.Error()
	}

	// Roll impact: realized on close (current value, marked at mid) flows into
	// the new structure; net new credit is the difference.
	netImpact := 0.0
	if plan != nil {
		// new position value at open ≈ net credit/paid of the new structure.
		newNet := plan.NetCredit
		netImpact = newNet - closeValue
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": s.ID, "symbol": s.Symbol, "type": s.Type, "display_name": s.DisplayName,
		"qty": s.Qty, "current_expiry": s.Expiry, "current_pnl": posPnL(pos),
		"current": current, "close_value": math.Round(closeValue*100) / 100,
		"series_options": seriesList, "target_series": targetExpiry,
		"chain": chainItems, "plan": plan, "plan_error": planErr,
		"net_impact":           math.Round(netImpact*100) / 100,
		"underlying_orderbook": underlyingOrderbook,
		"underlying_depth_src": underlyingDepthSrc,
	})
}

// posPnL returns the P&L of a position (0 if nil).
func posPnL(p *quant.Position) float64 {
	if p == nil {
		return 0
	}
	return math.Round(p.PnL*100) / 100
}

// optionQuoteForDepth returns the cached quote (mark/bid/ask) for an option,
// preferring a fresh live row.
func optionQuoteForDepth(secid string) optionQuoteEx {
	q, ok := cachedOptionQuoteEx(secid)
	if !ok {
		return optionQuoteEx{}
	}
	return q
}

// optionChainMenu lists the tradeable strikes of a leg (side/call) in a series
// with full top-of-book liquidity for each strike and a best-effort Alor depth.
func optionChainMenu(symbol, expiry, side string, isCall bool, anchorStrike float64) []map[string]interface{} {
	_, find, err := optionChainFor(symbol, expiry)
	if err != nil {
		return nil
	}
	// Walk strikes around the current ATM using the chain finder.
	chain := moexOptionsForAsset(symbol, expiry)
	strikes := []float64{}
	seen := map[float64]bool{}
	for _, o := range chain {
		if o.IsCall == isCall && !seen[o.Strike] {
			seen[o.Strike] = true
			strikes = append(strikes, o.Strike)
		}
	}
	sort.Float64s(strikes)
	// Keep a window of strikes around the anchor leg so the dropdown stays usable.
	const half = 12
	if anchorStrike > 0 && len(strikes) > 2*half {
		best := 0
		for i, st := range strikes {
			if st <= anchorStrike {
				best = i
			}
		}
		lo := best - half
		if lo < 0 {
			lo = 0
		}
		hi := best + half
		if hi > len(strikes) {
			hi = len(strikes)
		}
		strikes = strikes[lo:hi]
	}
	out := []map[string]interface{}{}
	for _, st := range strikes {
		opt := find(st, isCall)
		if opt == nil {
			continue
		}
		q := optionQuoteForDepth(opt.SecID)
		spreadPct := 0.0
		if q.Bid > 0 && q.Offer >= q.Bid {
			mid := (q.Bid + q.Offer) / 2
			if mid > 0 {
				spreadPct = math.Round((q.Offer-q.Bid)/mid*10000) / 100
			}
		}
		item := map[string]interface{}{
			"secid": opt.SecID, "strike": st, "is_call": isCall, "side": side,
			"mid":        math.Round(q.Price*10000) / 10000,
			"bid":        math.Round(q.Bid*10000) / 10000,
			"ask":        math.Round(q.Offer*10000) / 10000,
			"spread_pct": spreadPct,
			"depth":      []orderBookLevel{}, "depth_src": "iss",
		}
		// Best-effort Alor depth for this option via its ISS secid as symbol.
		if alorMarket != nil {
			if ob, err := alorMarket.FetchOrderbook("MOEX", opt.SecID); err == nil {
				levels := []orderBookLevel{}
				for _, b := range ob.Bids {
					levels = append(levels, orderBookLevel{Price: b.Price, Volume: b.Volume, Side: "bid"})
				}
				for _, a := range ob.Asks {
					levels = append(levels, orderBookLevel{Price: a.Price, Volume: a.Volume, Side: "ask"})
				}
				item["depth"] = levels
				item["depth_src"] = "alor"
			}
		}
		out = append(out, item)
	}
	return out
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
