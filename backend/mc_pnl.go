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

	// Determine spread direction: credit > 0 means short premium (credit spread).
	width := math.Abs(longK - shortK)
	// If both strikes coincide (or are missing), derive the spread width from
	// the entered max loss so the simulation stays meaningful.
	if width == 0 && maxLoss > 0 {
		width = math.Abs(maxLoss)
	}
	if width == 0 {
		width = math.Abs(credit) + 1
	}
	isDebit := credit < 0
	absCredit := math.Abs(credit)

	pnls := make([]float64, n)
	// Risk-neutral daily log step: iv annualized, DTE in calendar days.
	sig := iv / math.Sqrt(365)

	for i := 0; i < n; i++ {
		// Simulate daily spot path with GBM (zero drift, risk-neutral).
		s := spot
		for d := 0; d < dte; d++ {
			s *= math.Exp(sig * rand.NormFloat64())
		}

		// Spread P&L at expiry (European payoff), direction-aware by strikes:
		// width = |shortK - longK| caps every branch. Credit spread loses when
		// the underlying breaches the short leg; debit spread earns when it
		// breaches the long (money) leg.
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
	probs := 0.0
	sum := 0.0
	for _, p := range pnls {
		if p > 0 {
			probs++
		}
		sum += p
	}

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
