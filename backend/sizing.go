package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"option-quant-ai/quant"
)

// sizingResult is the computed recommended position size for a strategy.
type sizingResult struct {
	Symbol          string  `json:"symbol"`
	Strategy        string  `json:"strategy"`
	StrategyName    string  `json:"strategy_name"`
	Spot            float64 `json:"spot_price"`
	MaxLossPerContract float64 `json:"max_loss_per_contract"` // points
	MaxLossRub      float64 `json:"max_loss_rub"`            // RUB per contract
	MarginPerContract float64 `json:"margin_per_contract"`   // GO per contract
	MaxProfitRub    float64 `json:"max_profit_rub"`
	RiskBudgetPct   float64 `json:"risk_budget_pct"`
	RiskBudgetRub   float64 `json:"risk_budget_rub"`
	StopLossPct     float64 `json:"stop_loss_pct"` // % of max loss taken before exit
	QtyByRisk       int     `json:"qty_by_risk"`
	QtyByMargin     int     `json:"qty_by_margin"`
	RecommendedQty  int     `json:"recommended_qty"`
	RiskPerRecommendedRub float64 `json:"risk_per_recommended_rub"`
	MarginPerRecommendedRub float64 `json:"margin_per_recommended_rub"`
	Cash            float64 `json:"cash"`
	LockedMargin    float64 `json:"locked_margin"`
	InitialCapital  float64 `json:"initial_capital"`
	Feasible        bool    `json:"feasible"`
	Warnings        []string `json:"warnings"`
	MaxLots         int     `json:"max_lots"`
}

// positionSizingHandler computes how many contracts of a strategy fit the risk
// budget. Risk per contract = max loss of the strategy (in points) scaled by
// the contract multiplier and clipped by the stop-loss level, i.e. the realized
// loss if the stop fires. The result is also capped by available margin.
func positionSizingHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	q := r.URL.Query()
	symbol := q.Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	strategy := q.Get("strategy")
	if strategy == "" {
		strategy = "iron_condor"
	}

	riskBudgetPct := 2.0
	if v := q.Get("risk_budget_pct"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			riskBudgetPct = f
		}
	}
	stopLossPct := 25.0
	if v := q.Get("stop_loss_pct"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			stopLossPct = f
		}
	}
	maxLots := 500
	if v := q.Get("max_lots"); v != "" {
		if f, err := strconv.Atoi(v); err == nil && f > 0 {
			maxLots = f
		}
	}

	portfolio := quant.GetPortfolio()
	capital := portfolio.InitialCapital
	cash := portfolio.Cash
	locked := portfolio.LockedMargin
	if capital <= 0 {
		capital = 1000000.0
		cash = capital
	}

	res := sizingResult{
		Symbol:         symbol,
		Strategy:       strategy,
		RiskBudgetPct:  riskBudgetPct,
		StopLossPct:    stopLossPct,
		Cash:           math.Round(cash),
		LockedMargin:   math.Round(locked),
		InitialCapital: math.Round(capital),
		MaxLots:        maxLots,
		Warnings:       []string{},
	}

	// Strategy economics (per contract) from the live build.
	build := buildStrategy(symbol, strategy)
	if errMsg, ok := build["error"].(string); ok {
		res.Warnings = append(res.Warnings, errMsg)
		res.Feasible = false
		json.NewEncoder(w).Encode(res)
		return
	}
	if name, ok := build["strategy_name"].(string); ok {
		res.StrategyName = name
	}
	if s, ok := build["spot_price"].(float64); ok {
		res.Spot = s
	}
	maxLossPts := 0.0
	if v, ok := build["max_loss"].(float64); ok {
		maxLossPts = v
	}
	maxProfitPts := 0.0
	if v, ok := build["max_profit"].(float64); ok {
		maxProfitPts = v
	}
	marginPts := 0.0
	if v, ok := build["margin_short_total"].(float64); ok {
		marginPts = v
	}

	mult := contractMultiplier(symbol)
	res.MaxLossPerContract = math.Round(maxLossPts*100) / 100
	res.MaxLossRub = math.Round(maxLossPts * mult)
	res.MaxProfitRub = math.Round(maxProfitPts * mult)
	// margin_short_total from buildStrategy is already in RUB (IMNP is the
	// ruble margin for the short side). For long-only strategies it is 0;
	// fall back to the premium paid so the size is still capped.
	res.MarginPerContract = math.Round(marginPts)
	if res.MarginPerContract <= 0 {
		res.MarginPerContract = res.MaxLossRub
	}

	// Risk budget in RUB.
	riskRub := capital * riskBudgetPct / 100.0
	res.RiskBudgetRub = math.Round(riskRub)

	// Realized loss when the stop fires: stopLossPct of the full max loss.
	stopRiskRub := res.MaxLossRub * stopLossPct / 100.0
	if stopRiskRub <= 0 {
		stopRiskRub = res.MaxLossRub
	}

	// Quantity by risk budget.
	qtyByRisk := 0
	if stopRiskRub > 0 {
		qtyByRisk = int(math.Floor(riskRub / stopRiskRub))
	}
	// Quantity by margin (available free cash for new positions).
	qtyByMargin := 0
	if res.MarginPerContract > 0 {
		qtyByMargin = int(math.Floor(math.Max(cash, 0) / res.MarginPerContract))
	}

	if qtyByRisk > maxLots {
		qtyByRisk = maxLots
	}
	if qtyByMargin > maxLots {
		qtyByMargin = maxLots
	}

	res.QtyByRisk = qtyByRisk
	res.QtyByMargin = qtyByMargin

	// Recommended = min of the two caps; at least 0.
	rec := qtyByRisk
	if qtyByMargin < rec {
		rec = qtyByMargin
	}
	res.RecommendedQty = rec
	res.RiskPerRecommendedRub = math.Round(stopRiskRub * float64(rec))
	res.MarginPerRecommendedRub = math.Round(res.MarginPerContract * float64(rec))

	res.Feasible = rec >= 1
	if !res.Feasible {
		if qtyByRisk < 1 {
			res.Warnings = append(res.Warnings,
				"Риск-бюджет слишком мал: даже 1 контракт превышает лимит риска (учтите стоп-лосс).")
		}
		if qtyByMargin < 1 {
			res.Warnings = append(res.Warnings,
				"Свободные средства меньше ГО одного контракта — увеличьте капитал или уменьшите позиции.")
		}
	}
	if rec < qtyByRisk {
		res.Warnings = append(res.Warnings, "Размер ограничен свободными средствами (ГО), а не риск-бюджетом.")
	}

	json.NewEncoder(w).Encode(res)
}