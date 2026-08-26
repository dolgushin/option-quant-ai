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

	if credit == 0 || maxLoss == 0 || spot == 0 || iv == 0 || dte <= 0 {
		json.NewEncoder(w).Encode(map[string]string{"error": "missing required params"})
		return
	}
	if n <= 0 || n > 50000 {
		n = 5000
	}

	// Determine spread direction: credit > 0 means short premium (credit spread).
	width := math.Abs(longK - shortK)
	if width == 0 {
		width = math.Abs(longK - shortK)
	}
	isDebit := credit < 0
	absCredit := math.Abs(credit)

	pnls := make([]float64, n)
	dt := float64(dte) / 365.0
	dailySigma := iv / math.Sqrt(252)

	for i := 0; i < n; i++ {
		// Simulate daily spot path with GBM.
		s := spot
		for d := 0; d < dte; d++ {
			z := rand.NormFloat64()
			s *= math.Exp((iv*iv/2-0.5*dailySigma*dailySigma)*dt/float64(dte) + dailySigma*z)
		}

		// Spread P&L at expiry.
		var pnl float64
		if isDebit {
			pnl = -absCredit // paid premium
			if longK > shortK {
				// Bull call spread: profit if spot > longK
				if s > shortK {
					pnl += math.Min(s-shortK, width)
				}
			} else {
				// Bear put spread: profit if spot < longK
				if s < shortK {
					pnl += math.Min(shortK-s, width)
				}
			}
		} else {
			pnl = absCredit // received premium
			if longK > shortK {
				// Bull put spread: loss if spot < longK
				if s < longK {
					pnl -= math.Min(longK-s, width)
				}
			} else {
				// Bear call spread: loss if spot > longK
				if s > longK {
					pnl -= math.Min(s-longK, width)
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

	// Build histogram (20 bins).
	lo, hi := pnls[0], pnls[len(pnls)-1]
	nbins := 20
	bins := make([]histBin, nbins)
	if hi > lo {
		w := (hi - lo) / float64(nbins)
		for i := range bins {
			bins[i] = histBin{From: math.Round((lo+float64(i)*w)*100) / 100, To: math.Round((lo+float64(i+1)*w)*100) / 100}
		}
		for _, p := range pnls {
			idx := int((p - lo) / w)
			if idx >= nbins {
				idx = nbins - 1
			}
			bins[idx].Count++
		}
	}

	rnd := func(v float64) float64 { return math.Round(v*100) / 100 }
	json.NewEncoder(w).Encode(mcPLResult{
		P5:         rnd(pnls[int(float64(n)*0.05)]),
		P25:        rnd(pnls[int(float64(n)*0.25)]),
		P50:        rnd(pnls[int(float64(n)*0.50)]),
		P75:        rnd(pnls[int(float64(n)*0.75)]),
		P95:        rnd(pnls[int(float64(n)*0.95)]),
		ProbProfit: math.Round(probs / float64(n) * 10000) / 100,
		AvgPnL:     rnd(sum / float64(n)),
		MaxLoss:    rnd(pnls[0]),
		MaxWin:     rnd(pnls[len(pnls)-1]),
		Histogram:  bins,
	})
}
