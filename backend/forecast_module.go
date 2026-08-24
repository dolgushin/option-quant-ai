package main

// Forecast module: forward-looking view built on the same closed-trade
// dataset the statistics module uses. Bootstrap Monte-Carlo equity
// projection, per-strategy expected value with uncertainty, and
// regime-conditioned recommendations (which strategies historically worked
// in the market conditions we are in right now).

import (
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strconv"

	"option-quant-ai/quant"
)

type mcPoint struct {
	Step int     `json:"step"`
	P5   float64 `json:"p5"`
	P25  float64 `json:"p25"`
	P50  float64 `json:"p50"`
	P75  float64 `json:"p75"`
	P95  float64 `json:"p95"`
}

type strategyForecast struct {
	statsRow
	Std      float64 `json:"std"`
	TStat    float64 `json:"t_stat"` // avg / (std/sqrt(n)); |t|>2 ≈ значимо
	WinProb  float64 `json:"win_prob"`
	Forecast string  `json:"forecast"` // текстовая оценка
}

type regimeAdvice struct {
	Trend string     `json:"trend"`
	Vol   string     `json:"vol"`
	Rows  []statsRow `json:"rows"` // исторические сделки в таких же условиях
	Note  string     `json:"note"`
}

type forecastResponse struct {
	Horizon       int                `json:"horizon"`
	Runs          int                `json:"runs"`
	Fan           []mcPoint          `json:"fan"`
	ProbProfit    float64            `json:"prob_profit"` // вероятность суммарного плюса за горизонт
	Strategies    []strategyForecast `json:"strategies"`
	CurrentRegime regimeAdvice       `json:"current_regime"`
	Note          string             `json:"note"`
}

// mcFan runs a bootstrap Monte-Carlo over the historical PnL pool and returns
// percentile bands of cumulative equity at checkpoints of the horizon.
func mcFan(pnls []float64, horizon, runs int, rnd *rand.Rand) []mcPoint {
	if len(pnls) == 0 || horizon <= 0 || runs <= 0 {
		return nil
	}
	checkpoints := 10
	if horizon < checkpoints {
		checkpoints = horizon
	}
	step := horizon / checkpoints
	if step == 0 {
		step = 1
	}
	fan := make([]mcPoint, 0, checkpoints)
	for c := 1; c <= checkpoints; c++ {
		n := c * step
		if n > horizon {
			n = horizon
		}
		finals := make([]float64, runs)
		for r := 0; r < runs; r++ {
			s := 0.0
			for k := 0; k < n; k++ {
				s += pnls[rnd.Intn(len(pnls))]
			}
			finals[r] = s
		}
		sort.Float64s(finals)
		pick := func(p float64) float64 {
			idx := int(p * float64(runs-1))
			return math.Round(finals[idx]*100) / 100
		}
		fan = append(fan, mcPoint{
			Step: n,
			P5:   pick(0.05), P25: pick(0.25), P50: pick(0.50), P75: pick(0.75), P95: pick(0.95),
		})
	}
	return fan
}

// probProfitMC estimates P(total PnL over horizon > 0) by bootstrap.
func probProfitMC(pnls []float64, horizon, runs int, rnd *rand.Rand) float64 {
	if len(pnls) == 0 {
		return 0
	}
	pos := 0
	for r := 0; r < runs; r++ {
		s := 0.0
		for k := 0; k < horizon; k++ {
			s += pnls[rnd.Intn(len(pnls))]
		}
		if s > 0 {
			pos++
		}
	}
	return math.Round(float64(pos)/float64(runs)*1000) / 1000
}

// buildStrategyForecasts aggregates per-strategy stats with uncertainty.
func buildStrategyForecasts(trades []quant.Trade) []strategyForecast {
	groups := map[string][]float64{}
	for _, t := range trades {
		groups[t.Strategy] = append(groups[t.Strategy], t.RealizedPnL)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]strategyForecast, 0, len(keys))
	for _, k := range keys {
		pnls := groups[k]
		n := float64(len(pnls))
		sum, wins := 0.0, 0
		for _, p := range pnls {
			sum += p
			if p > 0 {
				wins++
			}
		}
		mean := sum / n
		variance := 0.0
		for _, p := range pnls {
			variance += (p - mean) * (p - mean)
		}
		variance /= math.Max(n-1, 1)
		std := math.Sqrt(variance)
		sf := strategyForecast{Std: math.Round(std*100) / 100}
		sf.Key = k
		sf.Trades = len(pnls)
		sf.NetPnl = math.Round(sum*100) / 100
		sf.WinRate = math.Round(float64(wins)/n*1000) / 1000
		sf.AvgPnl = math.Round(mean*100) / 100
		sf.Expectancy = sf.AvgPnl
		sf.WinProb = sf.WinRate
		if std > 0 && n > 1 {
			sf.TStat = math.Round((mean/(std/math.Sqrt(n)))*100) / 100
		}
		switch {
		case n < 5:
			sf.Forecast = "мало сделок — оценка ненадёжна"
		case sf.TStat > 2 && mean > 0:
			sf.Forecast = "стабильно прибыльна — можно наращивать"
		case sf.TStat < -2:
			sf.Forecast = "стабильно убыточна — пересмотреть правила"
		default:
			sf.Forecast = "результат в шуме — нужно больше данных"
		}
		out = append(out, sf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Expectancy > out[j].Expectancy })
	return out
}

// buildRegimeAdvice picks historical trades whose entry context matches the
// current market regime.
func buildRegimeAdvice(trades []quant.Trade, trend, vol string) regimeAdvice {
	ra := regimeAdvice{Trend: trend, Vol: vol}
	var matched []quant.Trade
	for _, t := range trades {
		if t.TrendAtEntry == trend && (vol == "" || t.VolRegime == vol || t.VolRegime == "") {
			matched = append(matched, t)
		}
	}
	ra.Rows = computeBreakdown(matched, func(t quant.Trade) string { return t.Strategy })
	if len(matched) == 0 {
		ra.Note = "Исторических сделок в текущих условиях ещё нет — рекомендации появятся после накопления журнала."
	} else {
		ra.Note = "Ожидания основаны на сделках, открытых в таких же рыночных условиях."
	}
	return ra
}

// forecastHandler returns the Monte-Carlo projection and regime advice.
// URL: /api/v2/forecast?horizon=100&runs=3000
func forecastHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	horizon := 100
	if v := r.URL.Query().Get("horizon"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			horizon = n
		}
	}
	runs := 3000
	if v := r.URL.Query().Get("runs"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 20000 {
			runs = n
		}
	}

	trades := quant.GetTrades()
	resp := forecastResponse{Horizon: horizon, Runs: runs}

	if len(trades) == 0 {
		resp.Note = "Журнал сделок пуст — прогноз появится после первых закрытых сделок."
		json.NewEncoder(w).Encode(resp)
		return
	}

	pnls := make([]float64, 0, len(trades))
	for _, t := range trades {
		pnls = append(pnls, t.RealizedPnL)
	}

	// Fixed seed: reproducible bands for the same dataset.
	rnd := rand.New(rand.NewSource(42))
	resp.Fan = mcFan(pnls, horizon, runs, rnd)
	rnd2 := rand.New(rand.NewSource(42))
	resp.ProbProfit = probProfitMC(pnls, horizon, runs, rnd2)
	resp.Strategies = buildStrategyForecasts(trades)

	// Current regime from the dominant symbol in the journal.
	counts := map[string]int{}
	dominant := ""
	best := 0
	for _, t := range trades {
		counts[t.Symbol]++
		if counts[t.Symbol] > best {
			best = counts[t.Symbol]
			dominant = t.Symbol
		}
	}
	trend, vol := "", ""
	if closes, err := underlyingCloses(dominant); err == nil && len(closes) >= 55 {
		if ts := computeTrendStats(closes); ts != nil {
			trend = ts.Regime
		}
		if iv := currentATMIVRaw(dominant); iv > 0 {
			if hv := hvFromCloses(closes[len(closes)-21:], 20); hv > 0 {
				e := (iv - hv) * 100
				switch {
				case e >= 5:
					vol = "IV>HV"
				case e <= -5:
					vol = "IV<HV"
				default:
					vol = "нейтрально"
				}
			}
		}
	}
	resp.CurrentRegime = buildRegimeAdvice(trades, trend, vol)
	if resp.CurrentRegime.Trend == "" {
		resp.CurrentRegime.Trend = "не определён"
	}
	json.NewEncoder(w).Encode(resp)
}
