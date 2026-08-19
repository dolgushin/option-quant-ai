package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"option-quant-ai/quant"
)

// historicalCandle is a daily MOEX futures candle (open interest irrelevant here).
type historicalCandle struct {
	Date  time.Time
	Close float64
}

// fetchFutureHistory fetches daily closes for a FORTS futures secid over the
// given range using the public MOEX ISS candles endpoint (interval=24).
func fetchFutureHistory(secid, from, till string) ([]historicalCandle, error) {
	url := fmt.Sprintf("http://iss.moex.com/iss/engines/futures/markets/forts/boards/RFUD/securities/%s/candles.json?iss.meta=off&from=%s&till=%s&interval=24&candles.columns=begin,close", secid, from, till)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Candles struct {
			Data [][]interface{} `json:"data"`
		} `json:"candles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	var out []historicalCandle
	for _, row := range data.Candles.Data {
		if len(row) < 2 {
			continue
		}
		begin, ok1 := row[0].(string)
		close_, ok2 := row[1].(float64)
		if !ok1 || !ok2 || close_ <= 0 {
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", begin)
		if err != nil {
			t, err = time.Parse("2006-01-02", begin)
			if err != nil {
				continue
			}
		}
		out = append(out, historicalCandle{Date: t, Close: close_})
	}
	return out, nil
}

// backtestLegSpec mirrors the per-strategy strike layout used by buildStrategy.
type backtestLegSpec struct {
	offset  int
	isCall  bool
	isShort bool
}

// backtestStrategySpecs returns the legs for a strategy name.
func backtestStrategySpecs(strategy string) ([]backtestLegSpec, string) {
	switch strategy {
	case "iron_butterfly":
		return []backtestLegSpec{{0, false, true}, {0, true, true}, {-1, false, false}, {1, true, false}}, "Iron Butterfly"
	case "bull_put_spread":
		return []backtestLegSpec{{-1, false, true}, {-2, false, false}}, "Bull Put Spread"
	case "bear_call_spread":
		return []backtestLegSpec{{1, true, true}, {2, true, false}}, "Bear Call Spread"
	case "bull_call_spread":
		return []backtestLegSpec{{0, true, false}, {1, true, true}}, "Bull Call Spread"
	case "bear_put_spread":
		return []backtestLegSpec{{0, false, false}, {-1, false, true}}, "Bear Put Spread"
	case "long_straddle":
		return []backtestLegSpec{{0, false, false}, {0, true, false}}, "Long Straddle"
	case "long_strangle":
		return []backtestLegSpec{{-1, false, false}, {1, true, false}}, "Long Strangle"
	default:
		return []backtestLegSpec{{-1, false, true}, {-2, false, false}, {1, true, true}, {2, true, false}}, "Iron Condor"
	}
}

// backtestTrade is one simulated strategy trade.
type backtestTrade struct {
	EntryDate string  `json:"entry_date"`
	ExitDate  string  `json:"exit_date"`
	DaysHeld  int     `json:"days_held"`
	EntrySpot float64 `json:"entry_spot"`
	ExitSpot  float64 `json:"exit_spot"`
	NetCredit float64 `json:"net_credit"` // per contract (negative = debit paid)
	MaxRisk   float64 `json:"max_risk"`   // per contract
	Comm      float64 `json:"comm"`       // round-trip commissions per contract
	PnL       float64 `json:"pnl"`        // per contract realized P&L (net of comm)
	PnLPct    float64 `json:"pnl_pct"`    // PnL / max risk %
	Win       bool    `json:"win"`
	ExitType  string  `json:"exit_type"` // "hold" or "stop_loss" or "expiry"
}

// backtestResult aggregates all simulated trades.
type backtestResult struct {
	Symbol          string          `json:"symbol"`
	Series          string          `json:"series"`
	Strategy        string          `json:"strategy"`
	StrategyName    string          `json:"strategy_name"`
	Days            int             `json:"days"`
	HoldDays        int             `json:"hold_days"`
	IVUsed          float64         `json:"iv_used"`
	MinHV           float64         `json:"min_hv"`
	CommPerContract float64         `json:"comm_per_contract"`
	CommissionsTotal float64        `json:"commissions_total"`
	Trades          int             `json:"total_trades"`
	Wins            int             `json:"wins"`
	Losses          int             `json:"losses"`
	WinRate         float64         `json:"win_rate"`
	AvgWin          float64         `json:"avg_win"`
	AvgLoss         float64         `json:"avg_loss"`
	ProfitFactor    float64         `json:"profit_factor"`
	TotalPnL        float64         `json:"total_pnl"`
	MaxDrawdown     float64         `json:"max_drawdown"`
	EquityCurve     []equityPoint   `json:"equity_curve"`
	TradesDetail    []backtestTrade `json:"trades"`
	Note            string          `json:"note"`
}

type equityPoint struct {
	Date   string  `json:"date"`
	Equity float64 `json:"equity"`
}

// estimateOptionPrice prices an option with Black-Scholes (per contract, no
// multiplier) using the given spot, strike, time-to-expiry and IV.
func estimateOptionPrice(isCall bool, spot, strike, t, iv float64) float64 {
	if t <= 0 {
		return 0
	}
	g := quant.CalculateBlackScholes(isCall, spot, strike, t, 0.16, iv)
	return g.Price
}

// currentATMIV estimates the current market implied volatility of an ATM option
// for the given symbol using its live chain and spot. Returns 0 if unavailable.
func currentATMIV(symbol string) float64 {
	return currentATMIVImpl(symbol, true)
}

// currentATMIVRaw returns the unclamped ATM IV (raw market reading) used for
// IV Rank / Percentile, where the 0.20 clamp of currentATMIV would distort the
// distribution comparison.
func currentATMIVRaw(symbol string) float64 {
	return currentATMIVImpl(symbol, false)
}

func currentATMIVImpl(symbol string, clamp bool) float64 {
	spot, err := getSpotPrice(symbol)
	if err != nil || spot <= 0 {
		return 0
	}
	expiry := ""
	if e := currentSeriesExpiry(symbol); e != nil {
		expiry = e.Format("2006-01-02")
	}
	chain := moexOptionsForAsset(symbol, expiry)
	if len(chain) == 0 {
		return 0
	}
	atm := nearestStrike(chain, spot)
	days := dteInDays(expiry, time.Now())
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0
	rRate := 0.16

	// Average IV of the ATM call and put at the strike nearest to spot. Same-strike
// quotes keep put/call consistent (skew at different strikes would distort it).
	var ivSum float64
	n := 0
	for _, isCall := range []bool{true, false} {
		var opt *optionContract
		for i := range chain {
			o := &chain[i]
			if o.IsCall != isCall || o.Strike != atm {
				continue
			}
			opt = o
			break
		}
		if opt == nil {
			continue
		}
		last, bid, offer, _ := moexOptionQuote(opt.SecID)
		mid := 0.0
		if bid > 0 && offer > 0 {
			mid = (bid + offer) / 2
		}
		if mid <= 0 {
			mid = last
		}
		if mid <= 0 {
			mid = opt.PrevPrice
		}
		if mid <= 0 {
			continue
		}
		iv := quant.ImpliedVolatility(isCall, mid, spot, atm, t, rRate)
		if iv > 0 {
			ivSum += iv
			n++
		}
	}
	if n == 0 {
		return 0
	}
	iv := ivSum / float64(n)
	// MOEX mid-quotes are often stale after a spot move (put-call parity can be
	// badly broken), so clamp to a sane market range for FORTS index/futures.
	if clamp {
		if iv < 0.20 {
			iv = 0.20
		}
		if iv > 0.80 {
			iv = 0.80
		}
	}
	return math.Round(iv*10000) / 10000
}

// circuitBreaker blocks new entries when risk limits are breached:
//   - today's realized loss exceeds the daily limit (-20,000 RUB)
//   - realized drawdown from peak equity exceeds the drawdown limit (-35,000 RUB)
func circuitBreaker() map[string]interface{} {
	const dailyLossLimit = -20000.0
	const drawdownLimit = -35000.0
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	trades := quant.GetTrades()
	todayPnL := 0.0
	run := 0.0
	peak := 0.0
	dd := 0.0
	for _, tr := range trades {
		if tr.ClosedAt.After(dayStart) {
			todayPnL += tr.RealizedPnL
		}
		run += tr.RealizedPnL
		if run > peak {
			peak = run
		}
	}
	if run < peak {
		dd = run - peak
	}

	var reasons []string
	allowed := true
	if todayPnL < dailyLossLimit {
		allowed = false
		reasons = append(reasons, fmt.Sprintf("Дневной убыток %.0f ₽ < лимита %.0f ₽", todayPnL, dailyLossLimit))
	}
	if dd < drawdownLimit {
		allowed = false
		reasons = append(reasons, fmt.Sprintf("Просадка от пика %.0f ₽ < лимита %.0f ₽", dd, drawdownLimit))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "Лимиты риска не превышены")
	}
	return map[string]interface{}{
		"allowed":            allowed,
		"reasons":            reasons,
		"today_realized_pnl": todayPnL,
		"drawdown":           dd,
	}
}

// tradeGate evaluates the systematic entry rules derived from the backtest
// against the current market: DTE window, implied volatility level, and
// realized volatility regime. Returns whether a new entry is allowed plus the
// reasons (or the violated rules). Used to gate live recommendations.
func tradeGate(symbol string) map[string]interface{} {
	gate := map[string]interface{}{
		"allowed": true,
		"reasons": []string{"Все правила входа выполнены (DTE 20-60, IV ≥ 25%, IV Rank ≥ 30%, HV ≥ 15%)"},
	}

	// Circuit breaker: block new entries if today's realized loss exceeded the
	// threshold, or the realized drawdown from the equity peak is too deep.
	cb := circuitBreaker()
	if !cb["allowed"].(bool) {
		reasons := cb["reasons"].([]string)
		gate["allowed"] = false
		gate["circuit_breaker"] = true
		gate["reasons"] = reasons
		return gate
	}
	gate["circuit_breaker"] = false

	expiry := currentSeriesExpiry(symbol)
	if expiry == nil {
		gate["allowed"] = false
		gate["reasons"] = []string{"Нет серии опционов для актива"}
		return gate
	}
	dte := dteInDays(expiry.Format("2006-01-02"), time.Now())

	var reasons []string
	okDTE := dte >= 20 && dte <= 60
	if !okDTE {
		reasons = append(reasons, fmt.Sprintf("DTE %d вне окна 20-60", dte))
	}

	// Live implied vol of the ATM pair.
	iv := currentATMIV(symbol)
	okIV := iv >= 0.25
	if !okIV {
		reasons = append(reasons, fmt.Sprintf("IV %.1f%% < 25%%", iv*100))
	}

	// IV Rank / Percentile from trailing-year sample history. Uses the raw
	// (unclamped) ATM IV so the rank is comparable with the sampled history.
	rankStats := ivRankStats(symbol, currentATMIVRaw(symbol))
	okIVRank := true
	if rankStats["available"] == true {
		ivRank, _ := rankStats["iv_rank"].(float64)
		okIVRank = ivRank >= 30
		if !okIVRank {
			reasons = append(reasons, fmt.Sprintf("IV Rank %.0f%% < 30%%", ivRank))
		}
	}

	// Realized vol from daily futures closes.
	series := selectedSeries[symbol]
	hv := 0.0
	if series != "" {
		from := time.Now().AddDate(0, 0, -40).Format("2006-01-02")
		till := time.Now().Format("2006-01-02")
		if candles, err := fetchFutureHistory(series, from, till); err == nil && len(candles) >= 21 {
			hv = realizedVol(candles, len(candles)-1)
		}
	}
	okHV := hv >= 0.15
	if !okHV {
		reasons = append(reasons, fmt.Sprintf("Реализ. волат. %.1f%% < 15%%", hv*100))
	}

	allowed := okDTE && okIV && okIVRank && okHV
	gate["allowed"] = allowed
	gate["dte"] = dte
	gate["iv"] = iv
	gate["hv"] = math.Round(hv*10000) / 10000
	gate["iv_rank"] = rankStats["iv_rank"]
	gate["iv_percentile"] = rankStats["iv_percentile"]
	gate["iv_history_count"] = rankStats["count"]
	if allowed {
		gate["reasons"] = []string{"Все правила входа выполнены (DTE 20-60, IV ≥ 25%, IV Rank ≥ 30%, HV ≥ 15%)"}
	} else if len(reasons) == 0 {
		gate["reasons"] = []string{"Нет данных для проверки правил входа"}
	} else {
		gate["reasons"] = reasons
	}
	return gate
}

// tradeTrend evaluates a simple trend overlay on the futures daily closes:
// SMA-20 vs SMA-50 cross direction plus short-term slope. Returns regime
// (BULLISH / BEARISH / SIDEWAYS), strength, and the raw SMAs.
func tradeTrend(symbol string) map[string]interface{} {
	out := map[string]interface{}{
		"regime":  "SIDEWAYS",
		"strength": "neutral",
		"sma20":   0.0,
		"sma50":   0.0,
		"close":   0.0,
		"error":   nil,
	}
	series := selectedSeries[symbol]
	if series == "" {
		out["error"] = "нет серии фьючерса"
		return out
	}
	from := time.Now().AddDate(0, 0, -80).Format("2006-01-02")
	till := time.Now().Format("2006-01-02")
	candles, err := fetchFutureHistory(series, from, till)
	if err != nil || len(candles) < 51 {
		out["error"] = "недостаточно данных для тренда"
		return out
	}

	sma := func(n int) float64 {
		if len(candles) < n {
			return 0
		}
		s := 0.0
		for i := len(candles) - n; i < len(candles); i++ {
			s += candles[i].Close
		}
		return s / float64(n)
	}

	s20 := sma(20)
	s50 := sma(50)
	last := candles[len(candles)-1].Close
	prev := candles[len(candles)-5].Close
	slope := 0.0
	if prev > 0 {
		slope = (last - prev) / prev
	}

	regime := "SIDEWAYS"
	strength := "neutral"
	if s20 > s50*1.01 {
		regime = "BULLISH"
	} else if s20 < s50*0.99 {
		regime = "BEARISH"
	}
	if slope > 0.01 {
		strength = "rising"
	} else if slope < -0.01 {
		strength = "falling"
	}

	out["regime"] = regime
	out["strength"] = strength
	out["sma20"] = math.Round(s20)
	out["sma50"] = math.Round(s50)
	out["close"] = last
	out["slope_5d"] = math.Round(slope*10000) / 10000
	return out
}

// realizedVol computes trailing annualized realized volatility from daily
// closes ending at index `end` (inclusive), using a 20-trading-day window.
// Returns 0 if not enough data.
func realizedVol(candles []historicalCandle, end int) float64 {
	const n = 20
	if end+1 < n {
		return 0
	}
	sum := 0.0
	var logs []float64
	for i := end - n + 1; i <= end; i++ {
		if candles[i].Close <= 0 || candles[i-1].Close <= 0 {
			return 0
		}
		logs = append(logs, math.Log(candles[i].Close/candles[i-1].Close))
	}
	for _, l := range logs {
		sum += l
	}
	mean := sum / float64(len(logs))
	v := 0.0
	for _, l := range logs {
		d := l - mean
		v += d * d
	}
	v /= float64(len(logs) - 1)
	return math.Sqrt(v) * math.Sqrt(252)
}

// runStrategyBacktest simulates a strategy on historical daily closes of the
// current futures series. Each day in the window is a potential entry; the
// trade is held HoldDays calendar days (or until series expiry). Premiums are
// modeled with Black-Scholes using the passed IV, so results are a first-order
// approximation of what the strategy would have returned.
//
// Options:
//   - dteMin/dteMax: only enter when days-to-expiry is within this window.
//   - stopLossPct: exit early when P&L falls below -stopLossPct% of max risk.
//   - takeProfitPct: exit early when P&L reaches +takeProfitPct% of max risk.
//   - commPerContract: round-trip commission per contract (₽) deducted from P&L.
//   - minHV: skip entries when trailing 20-day realized volatility < minHV
//     (annualized), i.e. don't sell premium into a dead-vol regime.
func runStrategyBacktest(symbol, strategy string, days, holdDays int, iv float64, dteMin, dteMax int, stopLossPct, takeProfitPct float64, commPerContract, minHV float64) (*backtestResult, error) {
	seriesMu.Lock()
	seriesCode := selectedSeries[symbol]
	seriesMu.Unlock()
	if seriesCode == "" {
		return nil, fmt.Errorf("no series selected for %s", symbol)
	}

	expiryTime := currentSeriesExpiry(symbol)
	if expiryTime == nil {
		return nil, fmt.Errorf("no expiry for %s series %s", symbol, seriesCode)
	}
	expiry := expiryTime.Format("2006-01-02")

	till := time.Now().Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -(days + 10)).Format("2006-01-02")

	candles, err := fetchFutureHistory(seriesCode, from, till)
	if err != nil || len(candles) < 2 {
		return nil, fmt.Errorf("failed to load history for %s: %v", seriesCode, err)
	}

	chain := moexOptionsForAsset(symbol, expiry)
	if len(chain) == 0 {
		return nil, fmt.Errorf("option chain not available for %s %s", symbol, expiry)
	}

	// Unique sorted strikes from the current chain.
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

	// Strike step = median gap between adjacent strikes of the current chain.
	// Historical ATM is rounded to this step so the modeled legs stay near the
	// entry spot instead of drifting to today's (shifted) chain strikes.
	step := 0.0
	if len(strikes) >= 2 {
		var gaps []float64
		for i := 1; i < len(strikes); i++ {
			gaps = append(gaps, strikes[i]-strikes[i-1])
		}
		step = gaps[len(gaps)/2]
	}
	if step <= 0 {
		step = 500.0
	}

	specs, displayName := backtestStrategySpecs(strategy)
	if iv <= 0 {
		iv = currentATMIV(symbol)
		if iv <= 0 {
			iv = 0.30
		}
	}

	var result backtestResult
	result.Symbol = symbol
	result.Series = seriesCode
	result.Strategy = strategy
	result.StrategyName = displayName
	result.Days = days
	result.HoldDays = holdDays
	result.IVUsed = math.Round(iv*10000) / 10000
	result.MinHV = minHV
	result.CommPerContract = commPerContract
	result.Note = fmt.Sprintf("Модельные премии: Black-Scholes (r=16%%), спот = историческое закрытие фьючерса, страйки = шаг текущей цепочки вокруг исторического ATM, IV = заданная/текущая рыночная. Вход только при DTE в окне, выход = стоп/тейк или удержание hold дней. Комиссия: %.1f ₽/контракт в оба конца, %d ног. %s", commPerContract, len(specs), func() string { if minHV > 0 { return fmt.Sprintf("Фильтр входа: реализованная волатильность ≥ %.1f%%.", minHV*100) }; return "Фильтр входа по волатильности выключен." }())

	var equity []float64
	equityTotal := 0.0
	peak := 0.0
	maxDD := 0.0

	for i := 0; i < len(candles)-1; i++ {
		entry := candles[i]

		// Rebuild the strike grid at the actual entry spot (same layout).
		atm := math.Round(entry.Close/step) * step
		grid := make([]float64, len(specs))
		for k, sp := range specs {
			grid[k] = atm + float64(sp.offset)*step
		}
		valueAtEntry := func(spot, t float64) (float64, float64) {
			val := 0.0
			credit := 0.0
			for k, sp := range specs {
				strike := grid[k]
				if strike <= 0 {
					return 0, 0
				}
				p := estimateOptionPrice(sp.isCall, spot, strike, t, iv)
				if sp.isShort {
					val -= p
					credit += p
				} else {
					val += p
				}
			}
			return val, credit
		}

		// Time to expiry at entry.
		dte := int(expiryTime.Sub(entry.Date).Hours() / 24)
		if dte <= 0 || dte < dteMin || dte > dteMax {
			continue
		}
		// Vol regime filter: skip if trailing realized vol is too low to
		// justify selling premium (for credit strategies) or to profitably
		// hold long-vol (debit strategies) within the hold window.
		if minHV > 0 {
			hv := realizedVol(candles, i)
			if hv < minHV {
				continue
			}
		}
		tEntry := float64(dte) / 365.0

		// Wing width (distance between short and long on same side).
		wing := 0.0
		for _, a := range specs {
			if !a.isShort {
				continue
			}
			for _, b := range specs {
				if b.isShort || a.isCall != b.isCall {
					continue
				}
				w := math.Abs(float64(a.offset-b.offset) * step)
				if w > wing {
					wing = w
				}
			}
		}
		if wing <= 0 {
			wing = 1
		}

		entryVal, _ := valueAtEntry(entry.Close, tEntry)
		netCredit := entryVal // for credit strategies this is the initial credit

		// Max risk: for credit strategies = wing - credit; for debit (long)
		// strategies the risk is the premium paid, capped by wing width.
		maxRisk := wing - netCredit
		if netCredit < 0 { // debit paid, e.g. long straddle/strangle
			maxRisk = -netCredit
		}
		if maxRisk < 0 {
			maxRisk = netCredit
		}
		if maxRisk < 1 {
			maxRisk = 1
		}

		// Determine exit: scan each day in the hold window (or until expiry)
		// for stop-loss / take-profit; otherwise hold to the last day.
		exitIdx := -1
		exitType := "hold"
		last := i + holdDays
		maxJ := len(candles) - 1
		if last > maxJ {
			last = maxJ
		}

		for j := i + 1; j <= last; j++ {
			exit := candles[j]
			tExit := float64(int(expiryTime.Sub(exit.Date).Hours()/24)) / 365.0
			if tExit < 0 {
				tExit = 0
			}
			if exit.Date.After(*expiryTime) {
				exitType = "expiry"
				exitIdx = j
				break
			}
			exitVal, _ := valueAtEntry(exit.Close, tExit)

			pnl := exitVal - entryVal
			pnlPct := pnl / maxRisk * 100
			if stopLossPct > 0 && pnlPct <= -stopLossPct {
				exitType = "stop_loss"
				exitIdx = j
				break
			}
			if takeProfitPct > 0 && pnlPct >= takeProfitPct {
				exitType = "take_profit"
				exitIdx = j
				break
			}
			exitIdx = j
		}

		if exitIdx < 0 {
			continue
		}
		exit := candles[exitIdx]
		tExit := float64(int(expiryTime.Sub(exit.Date).Hours()/24)) / 365.0
		if tExit < 0 {
			tExit = 0
		}
		exitVal, _ := valueAtEntry(exit.Close, tExit)

		// Round-trip commission: each leg traded once to open and once to close.
		comm := commPerContract * float64(len(specs)) * 2
		result.CommissionsTotal += comm

		pnl := (exitVal - entryVal) - comm
		pnlPct := 0.0
		if maxRisk > 0 {
			pnlPct = pnl / maxRisk * 100
		}

		win := pnl > 0
		equityTotal += pnl
		if equityTotal > peak {
			peak = equityTotal
		}
		dd := peak - equityTotal
		if dd > maxDD {
			maxDD = dd
		}
		equity = append(equity, equityTotal)

		result.TradesDetail = append(result.TradesDetail, backtestTrade{
			EntryDate: entry.Date.Format("2006-01-02"),
			ExitDate:  exit.Date.Format("2006-01-02"),
			DaysHeld:  int(exit.Date.Sub(entry.Date).Hours() / 24),
			EntrySpot: math.Round(entry.Close*100) / 100,
			ExitSpot:  math.Round(exit.Close*100) / 100,
			NetCredit: math.Round(netCredit*100) / 100,
			MaxRisk:   math.Round(maxRisk*100) / 100,
			Comm:      math.Round(comm*100) / 100,
			PnL:       math.Round(pnl*100) / 100,
			PnLPct:    math.Round(pnlPct*100) / 100,
			Win:       win,
			ExitType:  exitType,
		})
	}

	for k, e := range equity {
		result.EquityCurve = append(result.EquityCurve, equityPoint{
			Date:   candles[k].Date.Format("2006-01-02"),
			Equity: math.Round(e*100) / 100,
		})
	}

	result.Trades = len(result.TradesDetail)
	var winTotal, lossTotal float64
	for _, t := range result.TradesDetail {
		if t.Win {
			result.Wins++
			winTotal += t.PnL
		} else {
			result.Losses++
			lossTotal += t.PnL
		}
	}
	if result.Trades > 0 {
		result.WinRate = float64(result.Wins) / float64(result.Trades) * 100
	}
	if result.Wins > 0 {
		result.AvgWin = winTotal / float64(result.Wins)
	}
	if result.Losses > 0 {
		result.AvgLoss = lossTotal / float64(result.Losses)
	}
	if lossTotal < 0 {
		result.ProfitFactor = -winTotal / lossTotal
	} else if winTotal > 0 {
		result.ProfitFactor = 99999
	}
	result.TotalPnL = math.Round(equityTotal*100) / 100
	result.MaxDrawdown = math.Round(maxDD*100) / 100

	return &result, nil
}

// backtestHandler runs a historical backtest for a TDSS strategy.
// URL: /api/v1/backtest?strategy=iron_condor&symbol=Si&days=60&hold=14&iv=0.35
func backtestHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	strategy := r.URL.Query().Get("strategy")
	if strategy == "" {
		strategy = "iron_condor"
	}
	days := 60
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 10 {
			days = n
		}
	}
	holdDays := 14
	if v := r.URL.Query().Get("hold"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			holdDays = n
		}
	}
	dteMin, dteMax := 0, 9999
	if v := r.URL.Query().Get("dtemin"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			dteMin = n
		}
	}
	if v := r.URL.Query().Get("dtemax"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			dteMax = n
		}
	}
	stopLossPct := 0.0
	if v := r.URL.Query().Get("stop"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 100 {
			stopLossPct = f
		}
	}
	takeProfitPct := 0.0
	if v := r.URL.Query().Get("tp"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 100 {
			takeProfitPct = f
		}
	}
	commPerContract := 2.0 // MOEX FORTS ~1₽/contract exchange + ~1₽ broker
	if v := r.URL.Query().Get("comm"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			commPerContract = f
		}
	}
	minHV := 0.0
	if v := r.URL.Query().Get("minhv"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			minHV = f
		}
	}
	iv := 0.0
	if v := r.URL.Query().Get("iv"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			iv = f
		}
	}

	if holdDays >= days {
		holdDays = days / 2
	}

	res, err := runStrategyBacktest(symbol, strategy, days, holdDays, iv, dteMin, dteMax, stopLossPct, takeProfitPct, commPerContract, minHV)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(res)
}