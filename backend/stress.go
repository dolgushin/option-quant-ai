package main

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"option-quant-ai/quant"
)

// stressScenario is one cell of the spot × IV shock matrix.
type stressScenario struct {
	SpotShock float64 `json:"spot_shock"` // decimal, e.g. -0.05
	IVShock   float64 `json:"iv_shock"`   // decimal, e.g. 0.20
	PnL       float64 `json:"pnl"`        // portfolio value change vs current mark, RUB
	PnLPercent float64 `json:"pnl_percent"` // relative to current portfolio value
}

// stressPositionResult shows a position's PnL under the worst scenario found.
type stressPositionResult struct {
	ID       string  `json:"id"`
	Strategy string  `json:"strategy"`
	Symbol   string  `json:"symbol"`
	PnL      float64 `json:"pnl"`
	PnLPercent float64 `json:"pnl_percent"`
}

// stressTestResponse is the full /api/v1/risk/stress payload.
type stressTestResponse struct {
	SpotShocks []float64 `json:"spot_shocks"`
	IVShocks   []float64 `json:"iv_shocks"`
	Matrix     []stressScenario `json:"matrix"`
	Worst      stressScenario   `json:"worst"`
	Best       stressScenario   `json:"best"`
	Positions  []stressPositionResult `json:"positions"`
	CurrentValue float64 `json:"current_value"`
}

// stressTestHandler reprices the portfolio and evaluates every spot×IV shock
// cell. Futures track the spot shock directly; option legs are repriced with
// the Black-Scholes model at shocked spot and shocked implied vol. The current
// IV of each option is recovered from its current market price first, so the
// IV shock is applied on top of the market-implied level.
func stressTestHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	positions := quant.GetActivePositions()
	for i := range positions {
		repricePosition(&positions[i])
		quant.SavePosition(positions[i])
	}

	rRate := 0.16
	now := time.Now()

	// Baseline portfolio value at current marks (used for % reporting).
	baseValue := 0.0
	for i := range positions {
		p := &positions[i]
		mult := contractMultiplier(p.Symbol)
		for _, leg := range p.Legs {
			dir := 1.0
			if leg.Side == "SELL" {
				dir = -1.0
			}
			baseValue += dir * leg.CurrentPrice * mult * float64(leg.Quantity)
		}
	}

	// Scenario grid.
	spotShocks := []float64{-0.10, -0.05, -0.02, 0, 0.02, 0.05, 0.10}
	ivShocks := []float64{-0.30, -0.20, -0.10, 0, 0.10, 0.20, 0.30}

	type legModel struct {
		dir   float64
		mult  float64
		qty   float64
		kind  string
		isCall bool
		strike float64
		price float64
		spot  float64
		iv    float64
		t     float64
	}

	// Precompute per-leg models (spot, IV, time-to-expiry) once.
	var legs []legModel
	for i := range positions {
		p := &positions[i]
		spot, _ := getSpotPrice(p.Symbol)
		mult := contractMultiplier(p.Symbol)
		days := dteInDays(p.Expiry, now)
		if days <= 0 {
			days = 30
		}
		t := float64(days) / 365.0
		for _, leg := range p.Legs {
			dir := 1.0
			if leg.Side == "SELL" {
				dir = -1.0
			}
			lm := legModel{
				dir:   dir,
				mult:  mult,
				qty:   float64(leg.Quantity),
				kind:  leg.Kind,
				isCall: leg.IsCall,
				strike: leg.Strike,
				price: leg.CurrentPrice,
				spot:  spot,
				t:     t,
			}
			if leg.Kind == "OPTION" {
				iv := quant.ImpliedVolatility(leg.IsCall, leg.CurrentPrice, spot, leg.Strike, t, rRate)
				if iv <= 0 {
					iv = 0.30
				}
				lm.iv = iv
			}
			legs = append(legs, lm)
		}
	}

	// Evaluate the grid.
	var matrix []stressScenario
	worst := stressScenario{PnL: math.Inf(1)}
	best := stressScenario{PnL: math.Inf(-1)}
	for _, ss := range spotShocks {
		for _, is := range ivShocks {
			shockPnL := 0.0
			for _, lm := range legs {
				var newPrice float64
				if lm.kind == "FUTURES" {
					newPrice = lm.price * (1 + ss)
				} else {
					newSpot := lm.spot * (1 + ss)
					newIV := lm.iv * (1 + is)
					if newIV <= 0 {
						newIV = 0.0001
					}
					g := quant.CalculateBlackScholes(lm.isCall, newSpot, lm.strike, lm.t, rRate, newIV)
					newPrice = g.Price
				}
				shockPnL += lm.dir * (newPrice - lm.price) * lm.mult * lm.qty
			}
			sc := stressScenario{
				SpotShock:  ss,
				IVShock:    is,
				PnL:        math.Round(shockPnL),
				PnLPercent: 0,
			}
			if baseValue != 0 {
				sc.PnLPercent = math.Round(sc.PnL/baseValue*10000) / 100
			}
			matrix = append(matrix, sc)
			if sc.PnL < worst.PnL {
				worst = sc
			}
			if sc.PnL > best.PnL {
				best = sc
			}
		}
	}

	// Per-position breakdown under the worst scenario.
	var posResults []stressPositionResult
	if worst.PnL != math.Inf(1) {
		for i := range positions {
			p := &positions[i]
			spot, _ := getSpotPrice(p.Symbol)
			mult := contractMultiplier(p.Symbol)
			days := dteInDays(p.Expiry, now)
			if days <= 0 {
				days = 30
			}
			t := float64(days) / 365.0
			posPnL := 0.0
			for _, leg := range p.Legs {
				dir := 1.0
				if leg.Side == "SELL" {
					dir = -1.0
				}
				var newPrice float64
				if leg.Kind == "FUTURES" {
					newPrice = leg.CurrentPrice * (1 + worst.SpotShock)
				} else {
					iv := quant.ImpliedVolatility(leg.IsCall, leg.CurrentPrice, spot, leg.Strike, t, rRate)
					if iv <= 0 {
						iv = 0.30
					}
					g := quant.CalculateBlackScholes(leg.IsCall, spot*(1+worst.SpotShock), leg.Strike, t, rRate, iv*(1+worst.IVShock))
					newPrice = g.Price
				}
				posPnL += dir * (newPrice - leg.CurrentPrice) * mult * float64(leg.Quantity)
			}
			pp := 0.0
			if p.CurrentValue != 0 {
				pp = posPnL / math.Abs(p.CurrentValue) * 100
			}
			posResults = append(posResults, stressPositionResult{
				ID:         p.ID,
				Strategy:   p.Strategy,
				Symbol:     p.Symbol,
				PnL:        math.Round(posPnL),
				PnLPercent: math.Round(pp*100) / 100,
			})
		}
	}

	resp := stressTestResponse{
		SpotShocks:   spotShocks,
		IVShocks:     ivShocks,
		Matrix:       matrix,
		Worst:        worst,
		Best:         best,
		Positions:    posResults,
		CurrentValue: math.Round(baseValue),
	}
	json.NewEncoder(w).Encode(resp)
}