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

	// Per-leg IV from the current mark.
	ivs := make([]float64, len(a.Legs))
	for i := range a.Legs {
		l := &a.Legs[i]
		if l.Kind == "FUTURES" || l.Current <= 0 {
			continue
		}
		iv := quant.ImpliedVolatility(l.IsCall, l.Current, spot, l.Strike, t, r)
		if iv <= 0 {
			iv = 0.30
		}
		if iv > 3 {
			iv = 3
		}
		ivs[i] = iv
		l.Iv = math.Round(iv*1000) / 10
	}

	// Per-leg statics at the current spot (greeks per underlying unit,
	// P&L in money).
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
			pn += dir * (g.Price - l.Entry) * q * mult
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
		legsIn       []analyticsLeg
		symbol, expiry string
		spot, mult   float64
		dte, qty     int
		central      float64
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

	a := buildSpreadAnalytics(symbol, expiry, spot, dte, mult, legsIn)
	a.Qty = qty
	a.Central = central
	json.NewEncoder(w).Encode(a)
}
