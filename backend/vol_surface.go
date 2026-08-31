package main

// Volatility surface endpoint: returns IV data by strike and expiry for
// visualising skew and term structure on the frontend.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"
)

type volSurfacePoint struct {
	Strike    float64 `json:"strike"`
	IV        float64 `json:"iv"`        // implied volatility, percent
	Type      string  `json:"type"`      // "call" or "put"
	DTE       int     `json:"dte"`       // days to expiry
	Expiry    string  `json:"expiry"`    // expiry date
	Moneyness float64 `json:"moneyness"` // strike / spot
}

type volSurfaceResponse struct {
	Symbol   string            `json:"symbol"`
	Spot     float64           `json:"spot"`
	Points   []volSurfacePoint `json:"points"`
	Expiries []string          `json:"expiries"` // unique sorted expiries
}

// volSurfaceHandler returns IV data for all strikes across multiple expiries.
// GET /api/v1/vol-surface?symbol=Si&expiries=3 (default: 3 nearest)
func volSurfaceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	numExpiries := 3
	if n := r.URL.Query().Get("expiries"); n != "" {
		var v int
		if _, err := fmt.Sscanf(n, "%d", &v); err == nil && v > 0 && v <= 8 {
			numExpiries = v
		}
	}

	spot, _ := getSpotPrice(symbol)
	if spot <= 0 {
		json.NewEncoder(w).Encode(volSurfaceResponse{Symbol: symbol})
		return
	}

	series := optionSeriesForSymbol(symbol)
	sort.Slice(series, func(i, j int) bool { return series[i].LastDelDate < series[j].LastDelDate })
	today := time.Now().Format("2006-01-02")

	var expiries []string
	for _, s := range series {
		if s.LastDelDate >= today {
			expiries = append(expiries, s.LastDelDate)
			if len(expiries) >= numExpiries {
				break
			}
		}
	}

	var points []volSurfacePoint
	seenExpiries := map[string]bool{}
	asset := optionCalcAsset(symbol)

	for _, exp := range expiries {
		if optCalc == nil {
			continue
		}
		seriesCode, err := optCalc.SeriesByExpiry(asset, exp)
		if err != nil {
			continue
		}
		board, err := optCalc.Board(asset, seriesCode)
		if err != nil || (len(board.Calls) == 0 && len(board.Puts) == 0) {
			continue
		}
		seenExpiries[exp] = true
		dte := dteInDays(exp, time.Now())

		for _, o := range board.Calls {
			ivPct := math.Round(o.Volatility*100) / 100
			if o.Volatility <= 0 || ivPct > 500 {
				continue
			}
			points = append(points, volSurfacePoint{
				Strike:    o.Strike,
				IV:        ivPct,
				Type:      "call",
				DTE:       dte,
				Expiry:    exp,
				Moneyness: math.Round(o.Strike/spot*10000) / 10000,
			})
		}
		for _, o := range board.Puts {
			ivPct := math.Round(o.Volatility*100) / 100
			if o.Volatility <= 0 || ivPct > 500 {
				continue
			}
			points = append(points, volSurfacePoint{
				Strike:    o.Strike,
				IV:        ivPct,
				Type:      "put",
				DTE:       dte,
				Expiry:    exp,
				Moneyness: math.Round(o.Strike/spot*10000) / 10000,
			})
		}
	}

	// Deduplicate expiries for response.
	var sortedExpiries []string
	for e := range seenExpiries {
		sortedExpiries = append(sortedExpiries, e)
	}
	sort.Strings(sortedExpiries)

	json.NewEncoder(w).Encode(volSurfaceResponse{
		Symbol:   symbol,
		Spot:     spot,
		Points:   points,
		Expiries: sortedExpiries,
	})
}
