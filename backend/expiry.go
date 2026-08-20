package main

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"option-quant-ai/quant"
)

// pinRiskLeg describes the pin-risk of one short (sold) option leg at expiry.
type pinRiskLeg struct {
	SecID       string  `json:"secid"`
	Side        string  `json:"side"`
	Strike      float64 `json:"strike"`
	IsCall      bool    `json:"is_call"`
	ZonePct     float64 `json:"zone_pct"`     // +/- zone around the strike
	ZonePts     float64 `json:"zone_pts"`     // absolute zone in points
	Probability float64 `json:"probability_pct"` // P(spot settles inside zone)
	Level       string  `json:"level"`        // LOW / MEDIUM / HIGH
}

// positionExpiryRisk is the per-position expiry & roll analysis.
type positionExpiryRisk struct {
	ID         string              `json:"id"`
	Strategy   string              `json:"strategy"`
	Symbol     string              `json:"symbol"`
	Expiry     string              `json:"expiry"`
	DTE        int                 `json:"dte"`
	PnLPercent float64             `json:"pnl_percent"`
	ExitAdvice quant.ExitAdvice    `json:"exit_advice"`
	RollAdvice rollAdviceOut       `json:"roll_advice"`
	PinRisk    []pinRiskLeg        `json:"pin_risk"`
	Overall    string              `json:"overall"` // OK / WARNING / CRITICAL
	Messages   []string            `json:"messages"`
}

type rollAdviceOut struct {
	Action       string `json:"action"`        // HOLD / ROLL_TO_NEXT
	NextSeries   string `json:"next_series,omitempty"`
	NextExpiry   string `json:"next_expiry,omitempty"`
	NextDTE      int    `json:"next_dte,omitempty"`
	Details      string `json:"details"`
}

// expiryRiskHandler analyzes every active position for:
//   - days-to-expiry pressure (exit advice)
//   - roll recommendation to the next futures series
//   - pin-risk of sold strikes (probability spot settles near the strike)
//
// URL: /api/v1/positions/expiry-risk
func expiryRiskHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	positions := quant.GetActivePositions()
	now := time.Now()
	out := make([]positionExpiryRisk, 0, len(positions))

	for i := range positions {
		p := &positions[i]
		repricePosition(p)
		quant.SavePosition(*p)

		pr := positionExpiryRisk{
			ID:         p.ID,
			Strategy:   p.Strategy,
			Symbol:     p.Symbol,
			Expiry:     p.Expiry,
			DTE:        dteInDays(p.Expiry, now),
			PnLPercent: p.PnLPercent,
			Messages:   []string{},
		}

		// 1) Exit advice using real position telemetry.
		netDelta := positionNetDelta(p)
		profitPct := p.PnLPercent
		if profitPct < 0 {
			profitPct = 0
		}
		pr.ExitAdvice = quant.AnalyzeExitTriggers(float64(pr.DTE), netDelta, profitPct)

		// 2) Roll recommendation: when DTE <= 10 suggest the next series.
		if pr.DTE <= 10 {
			roll := nextRollSeries(p.Symbol, p.Expiry)
			if roll.NextSeries != "" {
				pr.RollAdvice = rollAdviceOut{
					Action:     "ROLL_TO_NEXT",
					NextSeries: roll.NextSeries,
					NextExpiry: roll.NextExpiry,
					NextDTE:    roll.NextDTE,
					Details:    "Скоро экспирация: перенесите позицию в следующий месяц, чтобы сохранить тета-распад и избежать пин-риска.",
				}
			} else {
				pr.RollAdvice = rollAdviceOut{
					Action:  "HOLD",
					Details: "Следующая серия не найдена в списке контрактов — удерживайте до экспирации.",
				}
			}
		} else {
			pr.RollAdvice = rollAdviceOut{
				Action:  "HOLD",
				Details: "До экспирации достаточно времени, ролл не требуется.",
			}
		}

		// 3) Pin-risk for each sold option leg.
		hv := realizedVolForSymbol(p.Symbol)
		spot, _ := getSpotPrice(p.Symbol)
		step := strikeStepForSymbol(p.Symbol, p.Expiry)

		hasHighPin := false
		hasMediumPin := false
		for _, leg := range p.Legs {
			if leg.Kind != "OPTION" || leg.Side != "SELL" || leg.Strike <= 0 {
				continue
			}
			zonePts := step / 2.0
			if zonePts <= 0 {
				zonePts = leg.Strike * 0.005
			}
			zonePct := zonePts / leg.Strike * 100.0
			prob := pinRiskProbability(spot, leg.Strike, zonePts, float64(pr.DTE), hv)
			level := "LOW"
			if prob >= 25.0 {
				level = "HIGH"
				hasHighPin = true
			} else if prob >= 10.0 {
				level = "MEDIUM"
				hasMediumPin = true
			}
			pr.PinRisk = append(pr.PinRisk, pinRiskLeg{
				SecID:       leg.SecID,
				Side:        leg.Side,
				Strike:      leg.Strike,
				IsCall:      leg.IsCall,
				ZonePct:     math.Round(zonePct*100) / 100,
				ZonePts:     math.Round(zonePts*100) / 100,
				Probability: math.Round(prob*100) / 100,
				Level:       level,
			})
		}

		// 4) Overall severity.
		pr.Overall = "OK"
		if pr.DTE <= 10 {
			pr.Overall = "CRITICAL"
			pr.Messages = append(pr.Messages, "Менее 10 дней до экспирации — требуется решение: закрыть или ролл.")
		}
		if hasHighPin {
			pr.Overall = "CRITICAL"
			pr.Messages = append(pr.Messages, "Высокий пин-риск по проданным страйкам.")
		} else if hasMediumPin {
			if pr.Overall != "CRITICAL" {
				pr.Overall = "WARNING"
			}
			pr.Messages = append(pr.Messages, "Умеренный пин-риск — следите за короткими страйками.")
		}
		if pr.ExitAdvice.Severity == "CRITICAL" {
			pr.Overall = "CRITICAL"
		} else if pr.ExitAdvice.Severity == "WARNING" && pr.Overall != "CRITICAL" {
			pr.Overall = "WARNING"
		}

		out = append(out, pr)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"positions": out,
		"summary":   expirySummary(out),
		"note":      "Pin-risk = вероятность закрытия спота внутри ±0.5 шага страйка к экспирации (логнормальная модель, HV 20д).",
	})
}

// expirySummary aggregates per-position statuses.
func expirySummary(positions []positionExpiryRisk) map[string]interface{} {
	crit := 0
	warn := 0
	ok := 0
	roll := 0
	for _, p := range positions {
		switch p.Overall {
		case "CRITICAL":
			crit++
		case "WARNING":
			warn++
		default:
			ok++
		}
		if p.RollAdvice.Action == "ROLL_TO_NEXT" {
			roll++
		}
	}
	return map[string]interface{}{
		"total":      len(positions),
		"critical":   crit,
		"warning":    warn,
		"ok":         ok,
		"roll_now":   roll,
		"max_dte":    maxDTE(positions),
		"min_dte":    minDTE(positions),
	}
}

func maxDTE(positions []positionExpiryRisk) int {
	m := 0
	for _, p := range positions {
		if p.DTE > m {
			m = p.DTE
		}
	}
	return m
}

func minDTE(positions []positionExpiryRisk) int {
	if len(positions) == 0 {
		return 0
	}
	m := positions[0].DTE
	for _, p := range positions {
		if p.DTE < m {
			m = p.DTE
		}
	}
	return m
}

// positionNetDelta returns the net delta (in contracts) of a position.
func positionNetDelta(p *quant.Position) float64 {
	spot, _ := getSpotPrice(p.Symbol)
	rRate := 0.16
	days := dteInDays(p.Expiry, time.Now())
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0
	net := 0.0
	for _, leg := range p.Legs {
		dir := 1.0
		if leg.Side == "SELL" {
			dir = -1.0
		}
		qty := float64(leg.Quantity)
		if leg.Kind == "FUTURES" {
			net += dir * 1.0 * qty
			continue
		}
		iv := quant.ImpliedVolatility(leg.IsCall, leg.CurrentPrice, spot, leg.Strike, t, rRate)
		if iv <= 0 {
			iv = 0.30
		}
		g := quant.CalculateBlackScholes(leg.IsCall, spot, leg.Strike, t, rRate, iv)
		net += dir * g.Delta * qty
	}
	if len(p.Legs) > 0 {
		net /= float64(len(p.Legs))
	}
	return net
}

// nextRollSeries returns the futures series that expires after the given
// position expiry, using the MOEX contract list (newest first).
type nextSeries struct {
	NextSeries string
	NextExpiry string
	NextDTE    int
}

func nextRollSeries(symbol, currentExpiry string) nextSeries {
	contracts := futuresContractsForSymbol(symbol)
	cur, err := time.Parse("2006-01-02", currentExpiry)
	if err != nil {
		return nextSeries{}
	}
	// contracts sorted newest first → scan from the oldest side to find the
	// earliest contract expiring after currentExpiry.
	var best futuresContract
	found := false
	for _, c := range contracts {
		t, err := time.Parse("2006-01-02", c.LastDelDate)
		if err != nil {
			continue
		}
		if t.After(cur) {
			if !found || t.Before(mustParse(best.LastDelDate)) {
				best = c
				found = true
			}
		}
	}
	if !found {
		return nextSeries{}
	}
	return nextSeries{
		NextSeries: best.Code,
		NextExpiry: best.LastDelDate,
		NextDTE:    dteInDays(best.LastDelDate, time.Now()),
	}
}

func mustParse(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

// realizedVolForSymbol returns trailing 20-day annualized realized volatility
// of the currently selected futures series, falling back to 0.30.
func realizedVolForSymbol(symbol string) float64 {
	seriesMu.Lock()
	code := selectedSeries[symbol]
	seriesMu.Unlock()
	if code == "" {
		return 0.30
	}
	till := time.Now().Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -45).Format("2006-01-02")
	candles, err := fetchFutureHistory(code, from, till)
	if err != nil || len(candles) < 21 {
		return 0.30
	}
	hv := realizedVol(candles, len(candles)-1)
	if hv <= 0 {
		return 0.30
	}
	return hv
}

// strikeStepForSymbol returns the median strike gap for the given expiry chain.
func strikeStepForSymbol(symbol, expiry string) float64 {
	chain := moexOptionsForAsset(symbol, expiry)
	seen := map[float64]bool{}
	var strikes []float64
	for _, o := range chain {
		if !seen[o.Strike] {
			seen[o.Strike] = true
			strikes = append(strikes, o.Strike)
		}
	}
	for i := 0; i < len(strikes); i++ {
		for j := i + 1; j < len(strikes); j++ {
			if strikes[j] < strikes[i] {
				strikes[i], strikes[j] = strikes[j], strikes[i]
			}
		}
	}
	if len(strikes) < 2 {
		return 0
	}
	gaps := make([]float64, 0, len(strikes)-1)
	for i := 1; i < len(strikes); i++ {
		gaps = append(gaps, strikes[i]-strikes[i-1])
	}
	sortFloats(gaps)
	return gaps[len(gaps)/2]
}

func sortFloats(a []float64) {
	for i := 0; i < len(a); i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// pinRiskProbability returns the probability (in %) that the spot settles
// inside [K-zone, K+zone] at expiry, under a lognormal model with drift r=0.
func pinRiskProbability(spot, strike, zone, dte float64, hv float64) float64 {
	if dte <= 0 {
		dte = 1
	}
	if hv <= 0 {
		hv = 0.30
	}
	T := dte / 365.0
	sigma := hv
	if zone <= 0 {
		zone = strike * 0.005
	}
	kLo := strike - zone
	kHi := strike + zone
	if kLo <= 0 {
		kLo = strike * 0.9
	}

	// Under lognormal S_T ~ LN(ln S0 - 0.5σ²T, σ²T), P(S_T < K) = CND(z(K))
	// with z(K) = (ln(K/S0) + 0.5σ²T) / (σ√T).
	z := func(k float64) float64 {
		return (math.Log(k/spot) + 0.5*sigma*sigma*T) / (sigma * math.Sqrt(T))
	}
	prob := quant.CND(z(kHi)) - quant.CND(z(kLo))
	if prob < 0 {
		prob = 0
	}
	if prob > 100 {
		prob = 100
	}
	return prob * 100
}