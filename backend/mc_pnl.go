package main

// Monte Carlo P&L distribution simulator: runs N simulations of a spread trade
// and returns the P&L distribution with percentiles for the frontend histogram.

import (
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"time"

	"option-quant-ai/optioncalc"
)

type mcPLResult struct {
	P5         float64   `json:"p5"`          // 5th percentile P&L
	P25        float64   `json:"p25"`         // 25th percentile
	P50        float64   `json:"p50"`         // median
	P75        float64   `json:"p75"`         // 75th percentile
	P95        float64   `json:"p95"`         // 95th percentile
	ProbProfit float64   `json:"prob_profit"` // probability of profit %
	AvgPnL     float64   `json:"avg_pnl"`     // expected P&L
	MaxLoss    float64   `json:"max_loss"`    // max loss scenario
	MaxWin     float64   `json:"max_win"`     // max win scenario
	Histogram  []histBin `json:"histogram"`
}

type histBin struct {
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	Count int     `json:"count"`
}

// mcPLHandler runs a Monte Carlo simulation for a spread and returns the P&L
// distribution. GET /api/v1/mc-pnl?credit=500&maxloss=2000&spot=85000
// &iv=0.15&short=85000&long=84500&dte=14&n=5000
func mcPLHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	credit, _ := strconv.ParseFloat(r.URL.Query().Get("credit"), 64)
	maxLoss, _ := strconv.ParseFloat(r.URL.Query().Get("maxloss"), 64)
	spot, _ := strconv.ParseFloat(r.URL.Query().Get("spot"), 64)
	iv, _ := strconv.ParseFloat(r.URL.Query().Get("iv"), 64)
	shortK, _ := strconv.ParseFloat(r.URL.Query().Get("short"), 64)
	longK, _ := strconv.ParseFloat(r.URL.Query().Get("long"), 64)
	dte, _ := strconv.Atoi(r.URL.Query().Get("dte"))
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))

	// Optional MOEX Options Calculator sourcing: when a symbol+expiry is given
	// and the volatility / spot aren't, pull the series ATM IV and live spot so
	// the simulation uses the exchange's own data instead of manual inputs.
	symbol := r.URL.Query().Get("symbol")
	expiry := r.URL.Query().Get("expiry")
	if symbol != "" && expiry != "" && optCalc != nil {
		if iv == 0 {
			iv = mcMoexATMIV(symbol, expiry)
		}
		if spot == 0 {
			spot, _ = getSpotPrice(symbol)
		}
	}

	if credit == 0 || spot == 0 || iv == 0 || dte <= 0 {
		json.NewEncoder(w).Encode(map[string]string{"error": "missing required params"})
		return
	}
	if n <= 0 || n > 50000 {
		n = 5000
	}

	// Time-seeded paths keep the single calculator lively; the grid scan
	// below uses a fixed seed so variants are fairly comparable.
	pnls := simulateSpreadPnL(credit, maxLoss, spot, iv, shortK, longK, dte, n,
		rand.New(rand.NewSource(time.Now().UnixNano())))
	probs, sum := summarizePnL(pnls)

	// Build histogram (24 bins). Even a degenerate (single-value) distribution
	// must render: pad the range by one unit so the frontend gets a chart.
	lo, hi := pnls[0], pnls[len(pnls)-1]
	if hi <= lo {
		hi = lo + 1
	}
	nbins := 24
	bins := make([]histBin, nbins)
	bw := (hi - lo) / float64(nbins)
	if bw <= 0 {
		bw = 1
	}
	for i := range bins {
		bins[i] = histBin{From: math.Round((lo+float64(i)*bw)*100) / 100, To: math.Round((lo+float64(i+1)*bw)*100) / 100}
	}
	for _, p := range pnls {
		idx := int((p - lo) / bw)
		if idx < 0 {
			idx = 0
		}
		if idx >= nbins {
			idx = nbins - 1
		}
		bins[idx].Count++
	}

	rnd := func(v float64) float64 { return math.Round(v*100) / 100 }
	json.NewEncoder(w).Encode(mcPLResult{
		P5:         rnd(pnls[int(float64(n)*0.05)]),
		P25:        rnd(pnls[int(float64(n)*0.25)]),
		P50:        rnd(pnls[int(float64(n)*0.50)]),
		P75:        rnd(pnls[int(float64(n)*0.75)]),
		P95:        rnd(pnls[int(float64(n)*0.95)]),
		ProbProfit: math.Round(probs/float64(n)*10000) / 100,
		AvgPnL:     rnd(sum / float64(n)),
		MaxLoss:    rnd(pnls[0]),
		MaxWin:     rnd(pnls[len(pnls)-1]),
		Histogram:  bins,
	})
}

// spreadWidth derives the wing width from the strikes, falling back to the
// entered max loss (or the credit) when strikes coincide or are missing.
func spreadWidth(shortK, longK, maxLoss, credit float64) float64 {
	if w := math.Abs(longK - shortK); w > 0 {
		return w
	}
	if maxLoss > 0 {
		return math.Abs(maxLoss)
	}
	return math.Abs(credit) + 1
}

// simulateSpreadPnL runs n GBM expiry paths (zero drift, risk-neutral daily
// steps) and returns the sorted P&L array. Direction comes from the credit
// sign and strike orientation (bull put / bear call / bull call / bear put).
// Pure given rnd — unit-tested.
func simulateSpreadPnL(credit, maxLoss, spot, iv, shortK, longK float64, dte, n int, rnd *rand.Rand) []float64 {
	width := spreadWidth(shortK, longK, maxLoss, credit)
	isDebit := credit < 0
	absCredit := math.Abs(credit)
	sig := iv / math.Sqrt(365)

	pnls := make([]float64, n)
	for i := 0; i < n; i++ {
		s := spot
		for d := 0; d < dte; d++ {
			s *= math.Exp(sig * rnd.NormFloat64())
		}
		var pnl float64
		if isDebit {
			pnl = -absCredit // paid premium
			if longK < shortK {
				// Bull call: profit when spot rises above the long (lower) strike.
				if s > longK {
					pnl += math.Min(s-longK, width)
				}
			} else {
				// Bear put: profit when spot falls below the long (higher) strike.
				if s < longK {
					pnl += math.Min(longK-s, width)
				}
			}
		} else {
			pnl = absCredit // received premium
			if longK < shortK {
				// Bull put: loss when spot drops below the short (higher) strike.
				if s < shortK {
					pnl -= math.Min(shortK-s, width)
				}
			} else {
				// Bear call: loss when spot rallies above the short (lower) strike.
				if s > shortK {
					pnl -= math.Min(s-shortK, width)
				}
			}
		}
		pnls[i] = pnl
	}
	sort.Float64s(pnls)
	return pnls
}

// summarizePnL counts profitable paths and sums P&L over a sorted array.
func summarizePnL(pnls []float64) (probs, sum float64) {
	for _, p := range pnls {
		if p > 0 {
			probs++
		}
		sum += p
	}
	return probs, sum
}

type mcScanRow struct {
	ShortK     float64 `json:"short"`
	LongK      float64 `json:"long"`
	DTE        int     `json:"dte"`
	IV         float64 `json:"iv"`
	ProbProfit float64 `json:"prob_profit"`
	AvgPnL     float64 `json:"avg_pnl"`
	P50        float64 `json:"p50"`
	MinPnL     float64 `json:"min_pnl"`
	MaxPnL     float64 `json:"max_pnl"`
}

// scanSpreadGrid scores short×wing×DTE×IV combinations with a fixed seed so
// variants are fairly comparable, returning the top-N by expectancy
// (average P&L), ProbProfit second. Pure — unit-tested.
func scanSpreadGrid(credit, spot float64, shorts, wings []float64, dtes []int, ivs []float64, n, top int) []mcScanRow {
	rows := []mcScanRow{}
	for _, sh := range shorts {
		for _, w := range wings {
			if w <= 0 {
				continue
			}
			// Both orientations: long below (put-style) and above (call-style).
			for _, lo := range []float64{sh - w, sh + w} {
				for _, dte := range dtes {
					if dte <= 0 {
						continue
					}
					for _, iv := range ivs {
						if iv <= 0 {
							continue
						}
						rnd := rand.New(rand.NewSource(42))
						pnls := simulateSpreadPnL(credit, 0, spot, iv, sh, lo, dte, n, rnd)
						probs, sum := summarizePnL(pnls)
						rows = append(rows, mcScanRow{
							ShortK: sh, LongK: lo, DTE: dte, IV: iv,
							ProbProfit: math.Round(probs/float64(n)*10000) / 100,
							AvgPnL:     math.Round(sum/float64(n)*100) / 100,
							P50:        math.Round(pnls[n/2]*100) / 100,
							MinPnL:     math.Round(pnls[0]*100) / 100,
							MaxPnL:     math.Round(pnls[n-1]*100) / 100,
						})
					}
				}
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AvgPnL != rows[j].AvgPnL {
			return rows[i].AvgPnL > rows[j].AvgPnL
		}
		return rows[i].ProbProfit > rows[j].ProbProfit
	})
	if top > 0 && len(rows) > top {
		rows = rows[:top]
	}
	return rows
}

// mcScanHandler auto-calculates the Monte-Carlo grid and returns the best
// variants by expectancy. POST /api/v1/mc-scan
// {"credit":80,"spot":86200,"shorts":[...],"wings":[...],"dtes":[...],"ivs":[...],"n":1000,"top":8}
func mcScanHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		Credit float64   `json:"credit"`
		Spot   float64   `json:"spot"`
		Symbol string    `json:"symbol"`
		Shorts []float64 `json:"shorts"`
		Wings  []float64 `json:"wings"`
		DTEs   []int     `json:"dtes"`
		IVs    []float64 `json:"ivs"`
		N      int       `json:"n"`
		Top    int       `json:"top"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": "bad request"})
		return
	}
	spot := req.Spot
	if spot <= 0 && req.Symbol != "" {
		spot, _ = getSpotPrice(req.Symbol)
	}
	if req.Credit == 0 || spot <= 0 {
		json.NewEncoder(w).Encode(map[string]string{"error": "нужны кредит и спот (или инструмент)"})
		return
	}
	shorts := req.Shorts
	if len(shorts) == 0 {
		// Default axis: ±1/3/6% around spot.
		for _, pct := range []float64{-0.06, -0.03, -0.01, 0, 0.01, 0.03, 0.06} {
			shorts = append(shorts, math.Round(spot*(1+pct)))
		}
	}
	wings := req.Wings
	if len(wings) == 0 {
		wings = []float64{spot * 0.01, spot * 0.02, spot * 0.03}
	}
	dtes := req.DTEs
	if len(dtes) == 0 {
		dtes = []int{7, 14, 30}
	}
	ivs := req.IVs
	if len(ivs) == 0 {
		ivs = []float64{0.15, 0.25, 0.35}
	}
	n := req.N
	if n <= 0 || n > 5000 {
		n = 1000
	}
	top := req.Top
	if top <= 0 || top > 20 {
		top = 8
	}
	if len(shorts)*len(wings)*2*len(dtes)*len(ivs) > 2000 {
		json.NewEncoder(w).Encode(map[string]string{"error": "сетка больше 2000 комбинаций — сузьте оси"})
		return
	}
	rows := scanSpreadGrid(req.Credit, spot, shorts, wings, dtes, ivs, n, top)
	json.NewEncoder(w).Encode(map[string]interface{}{"rows": rows, "scanned": len(shorts) * len(wings) * 2 * len(dtes) * len(ivs)})
}

// mcMoexATMIV returns the near-the-money implied volatility (decimal) for a
// symbol+expiry from the MOEX Options Calculator book, or 0 if unavailable.
func mcMoexATMIV(symbol, expiry string) float64 {
	seriesCode, err := optCalc.SeriesByExpiry(optionCalcAsset(symbol), expiry)
	if err != nil {
		return 0
	}
	board, err := optCalc.Board(optionCalcAsset(symbol), seriesCode)
	if err != nil || (len(board.Calls) == 0 && len(board.Puts) == 0) {
		return 0
	}
	spot, _ := getSpotPrice(symbol)
	if spot <= 0 {
		return 0
	}
	best := 0.0
	bestDiff := math.MaxFloat64
	// Average call/put IV at the strike closest to spot.
	seen := map[float64]int{}
	ivSum := map[float64]float64{}
	consider := func(o optioncalc.BoardOption) {
		if o.Volatility <= 0 {
			return
		}
		d := math.Abs(o.Strike - spot)
		if d < bestDiff {
			bestDiff = d
			best = o.Strike
		}
		seen[o.Strike]++
		ivSum[o.Strike] += o.Volatility
	}
	for _, o := range board.Calls {
		consider(o)
	}
	for _, o := range board.Puts {
		consider(o)
	}
	iv, ok := ivSum[best]
	if !ok {
		return 0
	}
	return iv / float64(seen[best]) / 100
}
