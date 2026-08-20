package main

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"option-quant-ai/quant"
)

// pnlLegSnapshot is the per-leg state captured at the start of the attribution
// window (baseline) and at the latest repricing (last).
type pnlLegSnapshot struct {
	Price float64 `json:"price"` // leg price in quote points
	Spot  float64 `json:"spot"`  // underlying spot at capture time
	IV    float64 `json:"iv"`    // implied vol of the option at capture time
}

// pnlPositionSnapshot holds the leg snapshots of one position.
type pnlPositionSnapshot struct {
	Legs map[string]pnlLegSnapshot `json:"legs"` // keyed by SecID
}

// pnlSnapshotFile persists the attribution state across restarts so the daily
// window survives a redeploy: Baseline is frozen at the first capture of a day
// (start of the window), Last is refreshed on every repricing.
type pnlSnapshotFile struct {
	Date     string                         `json:"date"` // YYYY-MM-DD of the current window
	Baseline map[string]pnlPositionSnapshot `json:"baseline"`
	Last     map[string]pnlPositionSnapshot `json:"last"`
}

var (
	pnlSnapMu sync.Mutex
	pnlSnap   *pnlSnapshotFile
)

func pnlSnapshotPath() string {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	return filepath.Join(dir, "pnl_snapshot.json")
}

func loadPnlSnapshot() {
	pnlSnapMu.Lock()
	defer pnlSnapMu.Unlock()
	if pnlSnap != nil {
		return
	}
	pnlSnap = &pnlSnapshotFile{
		Baseline: map[string]pnlPositionSnapshot{},
		Last:     map[string]pnlPositionSnapshot{},
	}
	data, err := os.ReadFile(pnlSnapshotPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, pnlSnap)
	if pnlSnap.Baseline == nil {
		pnlSnap.Baseline = map[string]pnlPositionSnapshot{}
	}
	if pnlSnap.Last == nil {
		pnlSnap.Last = map[string]pnlPositionSnapshot{}
	}
}

func savePnlSnapshot() {
	if pnlSnap == nil {
		return
	}
	data, err := json.MarshalIndent(pnlSnap, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(pnlSnapshotPath()); dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	os.WriteFile(pnlSnapshotPath(), data, 0o644)
}

// legGreeksAt computes Black-Scholes greeks for an option leg using the given
// price (either its quote or the captured price), spot and IV.
func legGreeksAt(isCall bool, price, spot, strike, t, rRate float64) quant.OptionGreeks {
	iv := quant.ImpliedVolatility(isCall, price, spot, strike, t, rRate)
	if iv <= 0 {
		iv = 0.30
	}
	return quant.CalculateBlackScholes(isCall, spot, strike, t, rRate, iv)
}

// pnlAttributionLeg is the decomposed PnL of one leg for the window.
type pnlAttributionLeg struct {
	SecID     string  `json:"secid"`
	Kind      string  `json:"kind"`
	Side      string  `json:"side"`
	Quantity  int     `json:"quantity"`
	Actual    float64 `json:"actual_pnl"`
	Delta     float64 `json:"delta"`
	Gamma     float64 `json:"gamma"`
	Theta     float64 `json:"theta"`
	Vega      float64 `json:"vega"`
	Residual  float64 `json:"residual"`
	SpotMove  float64 `json:"spot_move"`
	IVMove    float64 `json:"iv_move"` // percentage points, not decimal
	Days      float64 `json:"days"`
	Baseline  bool    `json:"baseline"` // true if window fell back to entry state
}

// pnlAttributionPosition aggregates the decomposed PnL of one position.
type pnlAttributionPosition struct {
	ID       string              `json:"id"`
	Strategy string              `json:"strategy"`
	Symbol   string              `json:"symbol"`
	Expiry   string              `json:"expiry"`
	Actual   float64             `json:"actual_pnl"`
	Delta    float64             `json:"delta"`
	Gamma    float64             `json:"gamma"`
	Theta    float64             `json:"theta"`
	Vega     float64             `json:"vega"`
	Residual float64             `json:"residual"`
	Legs     []pnlAttributionLeg `json:"legs"`
}

// pnlAttributionResponse is the full endpoint payload.
type pnlAttributionResponse struct {
	Date        string                    `json:"date"`
	WindowStart string                    `json:"window_start"`
	Baseline    bool                      `json:"baseline"`
	Actual      float64                   `json:"actual_pnl"`
	Delta       float64                   `json:"delta"`
	Gamma       float64                   `json:"gamma"`
	Theta       float64                   `json:"theta"`
	Vega        float64                   `json:"vega"`
	Residual    float64                   `json:"residual"`
	Positions   []pnlAttributionPosition  `json:"positions"`
}

// pnlAttributionHandler reprices the portfolio, decomposes the PnL of each
// position into delta/gamma/theta/vega/residual contributions since the start
// of the daily window, and advances the snapshot state.
func pnlAttributionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	loadPnlSnapshot()

	positions := quant.GetActivePositions()
	for i := range positions {
		repricePosition(&positions[i])
		quant.SavePosition(positions[i])
	}

	now := time.Now()
	today := now.Format("2006-01-02")
	rRate := 0.16

	pnlSnapMu.Lock()
	// On the first call of a new day, freeze yesterday's last state as the
	// baseline (start of the daily window).
	if pnlSnap.Date != today {
		pnlSnap.Baseline = pnlSnap.Last
		if pnlSnap.Baseline == nil {
			pnlSnap.Baseline = map[string]pnlPositionSnapshot{}
		}
		pnlSnap.Date = today
	}
	windowStart := pnlSnap.Date
	baseline := pnlSnap.Baseline
	last := pnlSnap.Last
	if last == nil {
		last = map[string]pnlPositionSnapshot{}
	}
	pnlSnapMu.Unlock()

	// Build the fresh snapshot of the current state.
	fresh := map[string]pnlPositionSnapshot{}
	for _, p := range positions {
		snap := pnlPositionSnapshot{Legs: map[string]pnlLegSnapshot{}}
		spot, _ := getSpotPrice(p.Symbol)
		days := dteInDays(p.Expiry, now)
		if days <= 0 {
			days = 30
		}
		t := float64(days) / 365.0
		for _, leg := range p.Legs {
			iv := 0.0
			if leg.Kind == "OPTION" {
				iv = quant.ImpliedVolatility(leg.IsCall, leg.CurrentPrice, spot, leg.Strike, t, rRate)
			}
			snap.Legs[leg.SecID] = pnlLegSnapshot{Price: leg.CurrentPrice, Spot: spot, IV: iv}
		}
		fresh[p.ID] = snap
	}

	resp := pnlAttributionResponse{
		Date:        today,
		WindowStart: windowStart,
		Baseline:    len(baseline) == 0,
		Positions:   []pnlAttributionPosition{},
	}

	for _, p := range positions {
		posAtt := pnlAttributionPosition{
			ID:       p.ID,
			Strategy: p.Strategy,
			Symbol:   p.Symbol,
			Expiry:   p.Expiry,
			Legs:     []pnlAttributionLeg{},
		}
		mult := contractMultiplier(p.Symbol)
		days := dteInDays(p.Expiry, now)
		if days <= 0 {
			days = 30
		}
		t := float64(days) / 365.0

		prevSnap := baseline[p.ID]
		for _, leg := range p.Legs {
			dir := 1.0
			if leg.Side == "SELL" {
				dir = -1.0
			}
			qty := float64(leg.Quantity)

			att := pnlAttributionLeg{
				SecID:    leg.SecID,
				Kind:     leg.Kind,
				Side:     leg.Side,
				Quantity: leg.Quantity,
			}

			// Current and baseline states for this leg.
			curPrice := leg.CurrentPrice
			curSpot := 0.0
			if s, err := getSpotPrice(p.Symbol); err == nil {
				curSpot = s
			}

			prevPrice := curPrice
			prevSpot := curSpot
			prevIV := 0.0
			baselineLeg := false

			if ps, ok := prevSnap.Legs[leg.SecID]; ok {
				prevPrice = ps.Price
				prevSpot = ps.Spot
				prevIV = ps.IV
			} else {
				// Newly opened leg (no baseline yet): attribute from its entry
				// state so the window shows total PnL since open.
				baselineLeg = true
				prevPrice = leg.EntryPrice
				if leg.Kind == "OPTION" {
					prevIV = quant.ImpliedVolatility(leg.IsCall, leg.EntryPrice, curSpot, leg.Strike, t, rRate)
				}
			}

			// Actual leg PnL in RUB.
			actual := dir * (curPrice - prevPrice) * mult * qty
			att.Actual = math.Round(actual)

			// For futures legs the "spot" is the contract price itself.
			ds := curSpot - prevSpot
			if leg.Kind == "FUTURES" {
				ds = curPrice - prevPrice
			}
			att.SpotMove = math.Round(ds*100) / 100

			// Window length in days (for theta).
			var dstart time.Time
			if baselineLeg {
				dstart = p.OpenedAt
			} else {
				dstart, _ = time.Parse("2006-01-02", windowStart)
			}
			ddays := now.Sub(dstart).Hours() / 24.0
			if ddays < 0 {
				ddays = 0
			}
			att.Days = math.Round(ddays*100) / 100

			if leg.Kind == "FUTURES" {
				// Futures PnL is pure delta (price move × multiplier × qty).
				att.Delta = actual
				att.Residual = 0
			} else {
				// Greeks at the baseline state drive the linear attribution.
				g := legGreeksAt(leg.IsCall, prevPrice, prevSpot, leg.Strike, t, rRate)
				// IV move in percentage points.
				dIV := 0.0
				if prevIV > 0 {
					curIV := quant.ImpliedVolatility(leg.IsCall, curPrice, curSpot, leg.Strike, t, rRate)
					if curIV > 0 {
						dIV = (curIV - prevIV) * 100
					}
				}
				att.IVMove = math.Round(dIV*100) / 100

				// Contributions in RUB (theta is per day, vega per 1% vol).
				deltaPnL := dir * g.Delta * ds * mult * qty
				gammaPnL := dir * 0.5 * g.Gamma * ds * ds * mult * qty
				thetaPnL := dir * g.Theta * ddays * mult * qty
				vegaPnL := dir * g.Vega * dIV * mult * qty

				att.Delta = math.Round(deltaPnL)
				att.Gamma = math.Round(gammaPnL)
				att.Theta = math.Round(thetaPnL)
				att.Vega = math.Round(vegaPnL)
				att.Residual = math.Round(actual - deltaPnL - gammaPnL - thetaPnL - vegaPnL)
				att.Baseline = baselineLeg
			}

			posAtt.Actual += att.Actual
			posAtt.Delta += att.Delta
			posAtt.Gamma += att.Gamma
			posAtt.Theta += att.Theta
			posAtt.Vega += att.Vega
			posAtt.Residual += att.Residual
			posAtt.Legs = append(posAtt.Legs, att)
		}

		resp.Positions = append(resp.Positions, posAtt)
		resp.Actual += posAtt.Actual
		resp.Delta += posAtt.Delta
		resp.Gamma += posAtt.Gamma
		resp.Theta += posAtt.Theta
		resp.Vega += posAtt.Vega
		resp.Residual += posAtt.Residual
	}

	// Persist the fresh state as "last" so the next call can roll the window.
	pnlSnapMu.Lock()
	pnlSnap.Last = fresh
	savePnlSnapshot()
	pnlSnapMu.Unlock()

	json.NewEncoder(w).Encode(resp)
}