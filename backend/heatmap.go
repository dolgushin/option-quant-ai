package main

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"option-quant-ai/quant"
)

// heatCell is the aggregate greek exposure at a (strike, expiry) point.
type heatCell struct {
	Strike   float64 `json:"strike"`
	Expiry   string  `json:"expiry"`
	DeltaRub float64 `json:"delta_rub"` // ₽ per 1 point move
	GammaRub float64 `json:"gamma_rub"` // ₽ per point² (negative = short gamma)
	Positions int    `json:"positions"`
	Level    string  `json:"level"` // LOW / MEDIUM / HIGH / CRITICAL
}

// heatRow is one strike row across all expiries.
type heatRow struct {
	Strike      float64    `json:"strike"`
	DeltaRub    float64    `json:"delta_rub"`
	GammaRub    float64    `json:"gamma_rub"`
	Positions   int        `json:"positions"`
	Level       string     `json:"level"`
	Cells       []heatCell `json:"cells"`
}

// heatResponse is the full delta/gamma heatmap payload.
type heatResponse struct {
	Symbol       string     `json:"symbol"`
	Metric       string     `json:"metric"`
	Expiries     []string   `json:"expiries"`
	Rows         []heatRow  `json:"rows"`
	TotalDelta   float64    `json:"total_delta"`
	TotalGamma   float64    `json:"total_gamma"`
	MaxDeltaCell float64    `json:"max_delta_cell"`
	MaxGammaCell float64    `json:"max_gamma_cell"`
	WorstStrike  float64    `json:"worst_strike"`
	Note         string     `json:"note"`
}

// heatmapHandler builds a strike × expiry grid of net delta/gamma exposure for
// every active position, highlighting concentration danger zones.
//
// URL: /api/v1/risk/heatmap?symbol=Si
func heatmapHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	metric := r.URL.Query().Get("metric")
	if metric != "delta" && metric != "gamma" {
		metric = "gamma"
	}

	positions := quant.GetActivePositions()
	now := time.Now()
	rRate := 0.16

	type point struct {
		strike   float64
		expiry   string
		deltaRub float64
		gammaRub float64
	}

	var pts []point
	expirySet := map[string]bool{}
	strikeSet := map[float64]bool{}
	spot, _ := getSpotPrice(symbol)
	step := strikeStepForSymbol(symbol, "")

	for i := range positions {
		p := &positions[i]
		repricePosition(p)
		quant.SavePosition(*p)

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
			qty := float64(leg.Quantity)

			if leg.Kind == "FUTURES" {
				k := spot
				if step > 0 {
					k = math.Round(spot/step) * step
				}
				pts = append(pts, point{strike: k, expiry: p.Expiry, deltaRub: dir * qty * mult, gammaRub: 0})
				strikeSet[k] = true
				expirySet[p.Expiry] = true
				continue
			}

			iv := quant.ImpliedVolatility(leg.IsCall, leg.CurrentPrice, spot, leg.Strike, t, rRate)
			if iv <= 0 {
				iv = 0.30
			}
			g := quant.CalculateBlackScholes(leg.IsCall, spot, leg.Strike, t, rRate, iv)
			pts = append(pts, point{
				strike:   leg.Strike,
				expiry:   p.Expiry,
				deltaRub: dir * g.Delta * qty * mult,
				gammaRub: dir * g.Gamma * qty * mult,
			})
			strikeSet[leg.Strike] = true
			expirySet[p.Expiry] = true
		}
	}

	var strikes []float64
	for k := range strikeSet {
		strikes = append(strikes, k)
	}
	sort.Float64s(strikes)

	var expiries []string
	for e := range expirySet {
		expiries = append(expiries, e)
	}
	sort.Strings(expiries)

	// Aggregate per (strike, expiry).
	type agg struct {
		delta     float64
		gamma     float64
		positions int
	}
	grid := map[string]*agg{}
	key := func(strike float64, expiry string) string {
		return fmtKey(strike) + "|" + expiry
	}
	for _, pt := range pts {
		k := key(pt.strike, pt.expiry)
		a, ok := grid[k]
		if !ok {
			a = &agg{}
			grid[k] = a
		}
		a.delta += pt.deltaRub
		a.gamma += pt.gammaRub
		a.positions++
	}

	// Absolute maxima for relative danger thresholds.
	maxDelta := 0.0
	maxGamma := 0.0
	totalDelta := 0.0
	totalGamma := 0.0
	for _, a := range grid {
		if math.Abs(a.delta) > maxDelta {
			maxDelta = math.Abs(a.delta)
		}
		if math.Abs(a.gamma) > maxGamma {
			maxGamma = math.Abs(a.gamma)
		}
		totalDelta += a.delta
		totalGamma += a.gamma
	}

	rows := make([]heatRow, 0, len(strikes))
	worstStrike := 0.0
	worstScore := 0.0
	for _, s := range strikes {
		row := heatRow{Strike: s, Cells: make([]heatCell, 0, len(expiries))}
		for _, e := range expiries {
			a := grid[key(s, e)]
			if a == nil {
				a = &agg{}
			}
			level := heatLevel(a.gamma, a.delta, maxGamma, maxDelta)
			row.Cells = append(row.Cells, heatCell{
				Strike:    s,
				Expiry:    e,
				DeltaRub:  math.Round(a.delta*100) / 100,
				GammaRub:  math.Round(a.gamma*100) / 100,
				Positions: a.positions,
				Level:     level,
			})
			row.DeltaRub += a.delta
			row.GammaRub += a.gamma
			row.Positions += a.positions
		}
		row.DeltaRub = math.Round(row.DeltaRub*100) / 100
		row.GammaRub = math.Round(row.GammaRub*100) / 100
		row.Level = heatLevel(row.GammaRub, row.DeltaRub, maxGamma, maxDelta)
		rows = append(rows, row)

		// Worst single strike: highest negative gamma or highest |delta|.
		score := math.Abs(row.GammaRub)
		if score > worstScore {
			worstScore = score
			worstStrike = s
		}
	}

	json.NewEncoder(w).Encode(heatResponse{
		Symbol:       symbol,
		Metric:       metric,
		Expiries:     expiries,
		Rows:         rows,
		TotalDelta:   math.Round(totalDelta*100) / 100,
		TotalGamma:   math.Round(totalGamma*100) / 100,
		MaxDeltaCell: math.Round(maxDelta*100) / 100,
		MaxGammaCell: math.Round(maxGamma*100) / 100,
		WorstStrike:  worstStrike,
		Note:         "Греки в рублях: delta = ₽ на 1 пункт движения, gamma = ₽ на пункт². Отрицательная гамма (проданные опционы) выделяется как опасная.",
	})
}

// heatLevel maps aggregate greek exposure to a danger level using relative
// thresholds against the grid maximum (short gamma is treated as most risky).
func heatLevel(gammaRub, deltaRub, maxGamma, maxDelta float64) string {
	// Negative gamma is the primary danger signal (short vol / pin risk).
	if gammaRub < 0 && maxGamma > 0 {
		ratio := math.Abs(gammaRub) / maxGamma
		if ratio > 0.66 {
			return "CRITICAL"
		}
		if ratio > 0.33 {
			return "HIGH"
		}
		if ratio > 0.10 {
			return "MEDIUM"
		}
	}
	// Positive gamma and delta concentration.
	if maxGamma > 0 && math.Abs(gammaRub)/maxGamma > 0.66 {
		return "HIGH"
	}
	if maxDelta > 0 && math.Abs(deltaRub)/maxDelta > 0.66 {
		return "HIGH"
	}
	if maxGamma > 0 && math.Abs(gammaRub)/maxGamma > 0.33 {
		return "MEDIUM"
	}
	if maxDelta > 0 && math.Abs(deltaRub)/maxDelta > 0.33 {
		return "MEDIUM"
	}
	return "LOW"
}

// fmtKey renders a float strike without scientific notation for map keys.
func fmtKey(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}