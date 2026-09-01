package main

// MOEX-constructor-style analytics for a vertical spread: mark-to-market P&L
// curve (Black-Scholes at each leg's own IV) next to the expiry payoff, delta
// and theta curves, and per-leg greeks with a totals row. The curve builder is
// a pure function covered by hermetic tests; the handler only gathers inputs.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"option-quant-ai/optioncalc"
	"option-quant-ai/quant"
)

type analyticsLeg struct {
	SecID    string  `json:"secid"`
	Side     string  `json:"side"`
	Kind     string  `json:"kind"`
	Strike   float64 `json:"strike"`
	IsCall   bool    `json:"is_call"`
	Quantity int     `json:"quantity"`
	Entry    float64 `json:"entry"`
	Current  float64 `json:"current"`
	Theo     float64 `json:"theo"`
	PnL      float64 `json:"pnl"`
	Delta    float64 `json:"delta"`
	Gamma    float64 `json:"gamma"`
	Vega     float64 `json:"vega"`
	Theta    float64 `json:"theta"`
	Rho      float64 `json:"rho"`
	Iv       float64 `json:"iv"` // percent
	// Moex carries the exchange's own greeks/IV for this leg (filled by the
	// handler from the MOEX Options Calculator). When set, buildSpreadAnalytics
	// uses these authoritative values instead of backing them out locally.
	Moex *moexLegData `json:"-"`
}

// moexLegData holds a leg's values as reported by the MOEX Options Calculator.
type moexLegData struct {
	IV    float64 // implied volatility, decimal (0.30 = 30%)
	Theo  float64
	Delta float64
	Gamma float64
	Vega  float64
	Theta float64
	Rho   float64
}

type analyticsCurves struct {
	Spots     []float64 `json:"spots"`
	PnlNow    []float64 `json:"pnl_now"`
	PnlExpiry []float64 `json:"pnl_expiry"`
	DeltaNow  []float64 `json:"delta_now"`
	DeltaExp  []float64 `json:"delta_expiry"`
	ThetaNow  []float64 `json:"theta_now"`
	ThetaT1   []float64 `json:"theta_t1"`
	GammaNow  []float64 `json:"gamma_now"`
	VegaNow   []float64 `json:"vega_now"`
	RhoNow    []float64 `json:"rho_now"`
}

type spreadAnalytics struct {
	Symbol  string             `json:"symbol"`
	Expiry  string             `json:"expiry"`
	Spot    float64            `json:"spot"`
	DTE     int                `json:"dte"`
	Mult    float64            `json:"multiplier"`
	Qty     int                `json:"qty"`
	Central float64            `json:"central_strike"`
	Source  string             `json:"source"` // "moex-optcalc" or "local"
	Legs    []analyticsLeg     `json:"legs"`
	Totals  map[string]float64 `json:"totals"`
	Curves  analyticsCurves    `json:"curves"`
}

func intrinsicValue(S, strike float64, isCall bool) float64 {
	if isCall {
		return math.Max(S-strike, 0)
	}
	return math.Max(strike-S, 0)
}

// buildSpreadAnalytics computes per-leg greeks at the current spot and the
// P&L/greek curves across a spot grid. IV per leg is backed out from its
// current market price and held flat across the grid — the same simplification
// the MOEX constructor applies to skew.
func buildSpreadAnalytics(symbol, expiry string, spot float64, dte int, mult float64, legsIn []analyticsLeg) *spreadAnalytics {
	a := &spreadAnalytics{
		Symbol: symbol, Expiry: expiry, Spot: spot, DTE: dte, Mult: mult,
		Legs: legsIn, Totals: map[string]float64{},
	}
	t := float64(dte) / 365.0
	if t <= 0 {
		t = 1.0 / 365.0
	}
	t1 := float64(dte-1) / 365.0
	if t1 <= 0 {
		t1 = 1.0 / 3650.0
	}
	const r = 0.16

	// Per-leg IV: prefer the MOEX Options Calculator's own IV (authoritative
	// skew); fall back to backing IV out of the current mark locally.
	ivs := make([]float64, len(a.Legs))
	for i := range a.Legs {
		l := &a.Legs[i]
		if l.Kind == "FUTURES" {
			continue
		}
		if l.Moex != nil && l.Moex.IV > 0 {
			ivs[i] = l.Moex.IV
			l.Iv = math.Round(l.Moex.IV*1000) / 10
			continue
		}
		if l.Current <= 0 {
			continue
		}
		iv := quant.ImpliedVolatility(l.IsCall, l.Current, spot, l.Strike, t, r)
		shown := iv
		if iv < 0.02 {
			// Price below intrinsic (stale/crossed quote) — inversion is
			// meaningless; keep greeks on a 30% proxy and hide the IV.
			iv = 0.30
			shown = 0
		}
		if iv > 3 {
			iv = 3
			shown = 300
		}
		ivs[i] = iv
		if shown > 0 {
			l.Iv = math.Round(shown*1000) / 10
		}
	}

	// Per-leg statics at the current spot (greeks per underlying unit,
	// P&L in money). When MOEX data is present use the exchange's own greeks.
	for i := range a.Legs {
		l := &a.Legs[i]
		dir := 1.0
		if l.Side == "SELL" {
			dir = -1
		}
		q := float64(l.Quantity)
		l.PnL = math.Round(dir*(l.Current-l.Entry)*q*mult*100) / 100
		if l.Kind == "FUTURES" {
			l.Delta = math.Round(dir*q*10000) / 10000
			continue
		}
		if l.Moex != nil {
			l.Theo = math.Round(l.Moex.Theo*100) / 100
			l.Delta = math.Round(dir*l.Moex.Delta*q*10000) / 10000
			l.Gamma = math.Round(dir*l.Moex.Gamma*q*10000) / 10000
			l.Vega = math.Round(dir*l.Moex.Vega*q*10000) / 10000
			l.Theta = math.Round(dir*l.Moex.Theta*q*10000) / 10000
			l.Rho = math.Round(dir*l.Moex.Rho*q*10000) / 10000
			continue
		}
		g := quant.CalculateBlackScholes(l.IsCall, spot, l.Strike, t, r, ivs[i])
		l.Theo = g.Price
		l.Delta = math.Round(dir*g.Delta*q*10000) / 10000
		l.Gamma = math.Round(dir*g.Gamma*q*10000) / 10000
		l.Vega = math.Round(dir*g.Vega*q*10000) / 10000
		l.Theta = math.Round(dir*g.Theta*q*10000) / 10000
		l.Rho = math.Round(dir*g.Rho*q*10000) / 10000
	}

	for _, l := range a.Legs {
		a.Totals["pnl"] += l.PnL
		a.Totals["delta"] += l.Delta
		a.Totals["gamma"] += l.Gamma
		a.Totals["vega"] += l.Vega
		a.Totals["theta"] += l.Theta
		a.Totals["rho"] += l.Rho
	}
	for k, v := range a.Totals {
		a.Totals[k] = math.Round(v*10000) / 10000
	}

	// Spot grid: strikes ± padding, at least ±12% of spot.
	lo, hi := spot, spot
	for _, l := range a.Legs {
		if l.Strike > 0 {
			lo = math.Min(lo, l.Strike)
			hi = math.Max(hi, l.Strike)
		}
	}
	span := math.Max((hi-lo)*0.8, spot*0.12)
	lo, hi = lo-span*0.6, hi+span*0.6
	const N = 61

	// Reference BS prices for each option leg at the current spot. The "now"
	// P&L curve is anchored to the position's actual market P&L at the current
	// spot (Current-Entry) and only uses the theoretical prices to shape how
	// that P&L changes as spot moves — so the chart always agrees with the
	// totals/depth, not just a best-effort recomputation of the mark.
	refs := make([]float64, len(a.Legs))
	for i := range a.Legs {
		l := &a.Legs[i]
		if l.Kind == "FUTURES" {
			refs[i] = spot
			continue
		}
		refs[i] = quant.CalculateBlackScholes(l.IsCall, spot, l.Strike, t, r, ivs[i]).Price
	}
	c := analyticsCurves{
		Spots: make([]float64, N), PnlNow: make([]float64, N), PnlExpiry: make([]float64, N),
		DeltaNow: make([]float64, N), DeltaExp: make([]float64, N),
		ThetaNow: make([]float64, N), ThetaT1: make([]float64, N),
		GammaNow: make([]float64, N), VegaNow: make([]float64, N), RhoNow: make([]float64, N),
	}
	for k := 0; k < N; k++ {
		S := lo + (hi-lo)*float64(k)/float64(N-1)
		c.Spots[k] = math.Round(S*100) / 100
		var pn, pe, dn, de, tn, tt, gn, vn, rn float64
		for i := range a.Legs {
			l := &a.Legs[i]
			dir := 1.0
			if l.Side == "SELL" {
				dir = -1
			}
			q := float64(l.Quantity)
			if l.Kind == "FUTURES" {
				pn += dir * (S - l.Entry) * q * mult
				pe += dir * (S - l.Entry) * q * mult
				dn += dir * q
				de += dir * q
				continue
			}
			g := quant.CalculateBlackScholes(l.IsCall, S, l.Strike, t, r, ivs[i])
			pn += dir * ((g.Price - refs[i]) + (l.Current - l.Entry)) * q * mult
			pe += dir * (intrinsicValue(S, l.Strike, l.IsCall) - l.Entry) * q * mult
			dn += dir * g.Delta * q
			gExp := quant.CalculateBlackScholes(l.IsCall, S, l.Strike, 1.0/3650.0, r, ivs[i])
			de += dir * gExp.Delta * q
			tn += dir * g.Theta * q
			gT1 := quant.CalculateBlackScholes(l.IsCall, S, l.Strike, t1, r, ivs[i])
			tt += dir * gT1.Theta * q
			gn += dir * g.Gamma * q
			vn += dir * g.Vega * q
			rn += dir * g.Rho * q
		}
		c.PnlNow[k] = math.Round(pn*100) / 100
		c.PnlExpiry[k] = math.Round(pe*100) / 100
		c.DeltaNow[k] = math.Round(dn*10000) / 10000
		c.DeltaExp[k] = math.Round(de*10000) / 10000
		c.ThetaNow[k] = math.Round(tn*10000) / 10000
		c.ThetaT1[k] = math.Round(tt*10000) / 10000
		c.GammaNow[k] = math.Round(gn*10000) / 10000
		c.VegaNow[k] = math.Round(vn*10000) / 10000
		c.RhoNow[k] = math.Round(rn*10000) / 10000
	}
	a.Curves = c
	return a
}

// spreadAnalyticsHandler returns MOEX-constructor-style analytics.
// URL: /api/v1/spreads/analytics?id=spr-...            (open position)
// URL: /api/v1/spreads/analytics?symbol=Si&type=bull_put&expiry=&qty=1  (plan)
func spreadAnalyticsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	id := r.URL.Query().Get("id")
	var (
		legsIn         []analyticsLeg
		symbol, expiry string
		spot, mult     float64
		dte, qty       int
		central        float64
	)

	if id != "" {
		s, ok := spreadRecordByID(id)
		if !ok || s.Status != "OPEN" {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "spread not found or not open"})
			return
		}
		pos, found := quant.GetPositionByID(s.PositionID)
		if !found {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "position not found"})
			return
		}
		repricePosition(pos)
		symbol, expiry = s.Symbol, s.Expiry
		dte = dteInDays(expiry, time.Now())
		mult = contractMultiplier(symbol)
		qty = s.Qty
		central = s.CentralStrike
		spot, _ = getSpotPrice(symbol)
		if spot <= 0 {
			spot = pos.CurrentValue
		}
		for _, l := range pos.Legs {
			legsIn = append(legsIn, analyticsLeg{
				SecID: l.SecID, Side: l.Side, Kind: l.Kind, Strike: l.Strike, IsCall: l.IsCall,
				Quantity: l.Quantity, Entry: l.EntryPrice, Current: l.CurrentPrice,
			})
		}
	} else {
		symbol = r.URL.Query().Get("symbol")
		if symbol == "" {
			symbol = "Si"
		}
		spreadType := r.URL.Query().Get("type")
		if spreadType == "" {
			spreadType = "bull_put"
		}
		expiry = r.URL.Query().Get("expiry")
		qty = 1
		if v := r.URL.Query().Get("qty"); v != "" {
			fmt.Sscanf(v, "%d", &qty)
		}
		plan, err := buildVerticalSpread(symbol, spreadType, expiry, qty)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
			return
		}
		symbol, expiry = plan.Symbol, plan.Expiry
		spot, mult = plan.Spot, plan.Multiplier
		dte, qty, central = plan.DaysToExp, plan.Qty, plan.CentralStrike
		for _, l := range plan.Legs {
			legsIn = append(legsIn, analyticsLeg{
				SecID: l.SecID, Side: l.Side, Kind: "OPTION", Strike: l.Strike, IsCall: l.IsCall,
				Quantity: plan.Qty, Entry: l.Price, Current: l.Price,
			})
		}
	}

	// Enrich legs with the MOEX Options Calculator's own greeks and IV where
	// the series is available. Falls back silently to local Black-Scholes.
	enriched, applied := enrichLegsFromMOEX(symbol, expiry, legsIn)

	// For an open position price the "now" P&L at the exchange's theoretical
	// price (the MOEX constructor's own mark), not the raw live mid — for
	// illiquid legs the live book is wide/stale and its mid is noise (e.g. an
	// ITM put must never be marked below intrinsic). Plans stay at entry (P&L 0).
	if id != "" {
		priceOpenLegsAtMOEXTheo(enriched)
	}

	a := buildSpreadAnalytics(symbol, expiry, spot, dte, mult, enriched)
	a.Qty = qty
	a.Central = central
	if applied {
		a.Source = "moex-optcalc"
	} else {
		a.Source = "local"
	}
	json.NewEncoder(w).Encode(a)
}

// priceOpenLegsAtMOEXTheo re-marks open-position option legs at the MOEX
// Options Calculator's theoretical price (used by the constructor for its own
// P&L). Mutates Current in place so the "now" P&L matches the exchange rather
// than a wide/stale live mid. Non-option legs and legs without MOEX data are
// left untouched.
func priceOpenLegsAtMOEXTheo(legs []analyticsLeg) {
	for i := range legs {
		l := &legs[i]
		if l.Kind != "OPTION" || l.Moex == nil || l.Moex.Theo <= 0 {
			continue
		}
		l.Current = l.Moex.Theo
	}
}

// enrichLegsFromMOEX attaches each option leg's greeks/IV from the MOEX
// Options Calculator board for the given series. Returns the (possibly
// unchanged) legs and reports whether the enrichment was applied. Share-premium
// options (equityOptions) and future-option assets not covered by the
// calculator are skipped and keep the local path.
func enrichLegsFromMOEX(symbol, expiry string, legs []analyticsLeg) (out []analyticsLeg, applied bool) {
	if optCalc == nil {
		return legs, false
	}
	asset := optionCalcAsset(symbol)
	seriesCode, err := optCalc.SeriesByExpiry(asset, expiry)
	if err != nil {
		return legs, false
	}
	board, err := optCalc.Board(asset, seriesCode)
	if err != nil {
		return legs, false
	}

	bySecid := map[string]*optioncalc.BoardOption{}
	for i := range board.Calls {
		bySecid[board.Calls[i].SecID] = &board.Calls[i]
	}
	for i := range board.Puts {
		bySecid[board.Puts[i].SecID] = &board.Puts[i]
	}

	out = make([]analyticsLeg, len(legs))
	copy(out, legs)
	found := false
	for i := range out {
		l := &out[i]
		if l.Kind == "FUTURES" {
			continue
		}
		bo, ok := bySecid[l.SecID]
		if !ok {
			// Fall back to matching by strike+type if secid differs.
			bo = findBoardOption(board, l.Strike, l.IsCall)
			if bo == nil {
				continue
			}
		}
		l.Moex = &moexLegData{
			IV:    bo.Volatility / 100,
			Theo:  bo.Theo,
			Delta: bo.Delta,
			Gamma: bo.Gamma,
			Vega:  bo.Vega,
			Theta: bo.Theta,
			Rho:   bo.Rho,
		}
		found = true
	}
	return out, found
}

// findBoardOption locates a call/put row by strike in a board.
func findBoardOption(board *optioncalc.Board, strike float64, isCall bool) *optioncalc.BoardOption {
	src := board.Puts
	if isCall {
		src = board.Calls
	}
	for i := range src {
		if math.Abs(src[i].Strike-strike) < 0.5 {
			return &src[i]
		}
	}
	return nil
}
