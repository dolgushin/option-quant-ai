package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Pre-trade entry advice for a vertical spread (KNOWLEDGE.md §6): a weighted
// checklist over DTE window, trend fit, volatility edge (IV vs HV + IV rank),
// credit quality and leg liquidity. The scoring is a pure function so it is
// covered by hermetic tests; the handler only gathers market inputs.

type adviceCheck struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // ok | warn | bad | info
	Detail string `json:"detail"`
	Weight int    `json:"weight"`
	Earned int    `json:"earned"`
}

type spreadAdvice struct {
	Symbol  string             `json:"symbol"`
	Type    string             `json:"type"`
	Expiry  string             `json:"expiry"`
	Spot    float64            `json:"spot"`
	DTE     int                `json:"dte"`
	Score   int                `json:"score"`
	Verdict string             `json:"verdict"` // РЕКОМЕНДУЕТСЯ / ОСТОРОЖНО / НЕ РЕКОМЕНДУЕТСЯ
	Summary string             `json:"summary"`
	Checks  []adviceCheck      `json:"checks"`
	Metrics map[string]float64 `json:"metrics"`
}

// adviceInputs carries everything the scorer needs.
type adviceInputs struct {
	spreadType     string
	IsDebit        bool
	DTE            int
	CreditPerShare float64 // NetCredit of the plan (>0 for credit spreads)
	Width          float64
	MaxProfit      float64
	MaxLoss        float64
	IvATM          float64
	Hv20           float64
	IvRankAvail    bool
	IvRank         float64
	TrendRegime    string  // BULLISH | BEARISH | SIDEWAYS | "" unknown
	TrendStrength  string  // rising | falling | neutral
	LiquidityPct   float64 // avg bid/ask width as % of mid across legs; -1 unknown
}

// ---- pure analytics over daily closes ----

func smaN(closes []float64, n int) float64 {
	if n <= 0 || len(closes) < n {
		return 0
	}
	s := 0.0
	for i := len(closes) - n; i < len(closes); i++ {
		s += closes[i]
	}
	return s / float64(n)
}

func rsiWilder(closes []float64, n int) float64 {
	if len(closes) < n+1 {
		return 50
	}
	gains, losses := 0.0, 0.0
	for i := len(closes) - n; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			gains += d
		} else {
			losses -= d
		}
	}
	if gains+losses == 0 {
		return 50
	}
	rs := gains / math.Max(losses, 1e-12)
	return 100 - 100/(1+rs)
}

func rocN(closes []float64, n int) float64 {
	i := len(closes) - 1 - n
	if i < 0 || closes[i] == 0 {
		return 0
	}
	return (closes[len(closes)-1] - closes[i]) / closes[i]
}

// hvFromCloses returns annualized realized volatility from the log returns of
// the last window+1 closes.
func hvFromCloses(closes []float64, window int) float64 {
	if len(closes) < window+1 {
		return 0
	}
	rets := make([]float64, 0, window)
	base := len(closes) - window - 1
	for i := base + 1; i < len(closes); i++ {
		if closes[i-1] > 0 && closes[i] > 0 {
			rets = append(rets, math.Log(closes[i]/closes[i-1]))
		}
	}
	if len(rets) < 5 {
		return 0
	}
	mean := 0.0
	for _, r := range rets {
		mean += r
	}
	mean /= float64(len(rets))
	variance := 0.0
	for _, r := range rets {
		variance += (r - mean) * (r - mean)
	}
	variance /= float64(len(rets) - 1)
	return math.Sqrt(variance) * math.Sqrt(252)
}

type trendStats struct {
	Close    float64
	SMA20    float64
	SMA50    float64
	Slope5   float64
	RSI14    float64
	ROC10    float64
	Regime   string // BULLISH | BEARISH | SIDEWAYS | ""
	Strength string // rising | falling | neutral
}

// computeTrendStats mirrors tradeTrend thresholds on a plain close series so
// both futures and share underlyings are supported.
func computeTrendStats(closes []float64) *trendStats {
	ts := &trendStats{Regime: "", Strength: "neutral"}
	n := len(closes)
	if n < 51 {
		return ts
	}
	ts.Close = closes[n-1]
	ts.SMA20 = smaN(closes, 20)
	ts.SMA50 = smaN(closes, 50)
	if closes[n-6] > 0 {
		ts.Slope5 = (closes[n-1] - closes[n-6]) / closes[n-6]
	}
	ts.RSI14 = rsiWilder(closes, 14)
	ts.ROC10 = rocN(closes, 10)
	switch {
	case ts.SMA20 > ts.SMA50*1.01:
		ts.Regime = "BULLISH"
	case ts.SMA20 < ts.SMA50*0.99:
		ts.Regime = "BEARISH"
	default:
		ts.Regime = "SIDEWAYS"
	}
	switch {
	case ts.Slope5 > 0.01:
		ts.Strength = "rising"
	case ts.Slope5 < -0.01:
		ts.Strength = "falling"
	}
	return ts
}

// ---- scoring ----

func scoreSpreadAdvice(in adviceInputs) *spreadAdvice {
	adv := &spreadAdvice{Checks: make([]adviceCheck, 0, 5), Metrics: map[string]float64{}}
	score := 0
	add := func(id, title, status, detail string, earned, weight int) {
		adv.Checks = append(adv.Checks, adviceCheck{
			ID: id, Title: title, Status: status, Detail: detail, Earned: earned, Weight: weight,
		})
		score += earned
	}

	// 1) DTE window (KNOWLEDGE.md §1.1): 14–45 sweet zone; weekly series may
	// start at 7; below that gamma risk dominates.
	switch {
	case in.DTE <= 0:
		add("dte", "Срок до экспирации", "bad", "нет данных по экспирации", 0, 15)
	case in.DTE < 7:
		add("dte", "Срок до экспирации", "bad", fmt.Sprintf("DTE %d < 7 — гамма-риск у экспирации", in.DTE), 3, 15)
	case in.DTE < 14:
		add("dte", "Срок до экспирации", "warn", fmt.Sprintf("DTE %d — последняя неделя серии", in.DTE), 9, 15)
	case in.DTE <= 45:
		add("dte", "Срок до экспирации", "ok", fmt.Sprintf("DTE %d в окне входа 14–45", in.DTE), 15, 15)
	default:
		add("dte", "Срок до экспирации", "warn", fmt.Sprintf("DTE %d > 45 — тета работает медленно", in.DTE), 9, 15)
	}

	// 2) Direction fit is appended by scoreTrendFit in the handler.

	// 3) Volatility edge (IV vs HV).
	rankTxt := ""
	if in.IvRankAvail {
		rankTxt = fmt.Sprintf(", IV Rank %.0f%%", in.IvRank)
	}
	switch {
	case in.IvATM > 0 && in.Hv20 > 0:
		edge := (in.IvATM - in.Hv20) * 100
		switch {
		case edge >= 5:
			if in.IsDebit {
				add("vol", "Волатильность", "warn", fmt.Sprintf("Премия дорогая (IV−HV=+%.1f п.п.%s) для покупки дебета", edge, rankTxt), 8, 25)
			} else {
				add("vol", "Волатильность", "ok", fmt.Sprintf("Премия дорогая (IV−HV=+%.1f п.п.%s) — продажа с запасом", edge, rankTxt), 25, 25)
			}
		case edge > -5:
			add("vol", "Волатильность", "info", fmt.Sprintf("IV≈HV (%+.1f п.п.%s) — преимущества по воле нет", edge, rankTxt), 15, 25)
		default:
			if in.IsDebit {
				add("vol", "Волатильность", "ok", fmt.Sprintf("Премия дешёвая (IV−HV=%.1f п.п.%s) — благоприятно покупке", edge, rankTxt), 23, 25)
			} else {
				add("vol", "Волатильность", "bad", fmt.Sprintf("Премия дешёвая (IV−HV=%.1f п.п.%s) — продавать невыгодно", edge, rankTxt), 4, 25)
			}
		}
	default:
		add("vol", "Волатильность", "info", "Нет данных IV/HV для оценки преимущества", 12, 25)
	}

	// 4) Credit/debit quality (KNOWLEDGE.md §1.1).
	switch {
	case in.CreditPerShare > 0 && in.Width > 0 && !in.IsDebit:
		q := in.CreditPerShare / in.Width * 100
		switch {
		case q >= 33:
			add("quality", "Качество кредита", "ok", fmt.Sprintf("Кредит %.0f%% ширины крыла (норма ≥33%%)", q), 20, 20)
		case q >= 22:
			add("quality", "Качество кредита", "warn", fmt.Sprintf("Кредит %.0f%% ширины крыла — маловато (норма ≥33%%)", q), 11, 20)
		default:
			add("quality", "Качество кредита", "bad", fmt.Sprintf("Кредит %.0f%% ширины крыла — риск не оправдан", q), 4, 20)
		}
	case in.IsDebit && in.MaxLoss > 0:
		rr := in.MaxProfit / in.MaxLoss
		switch {
		case rr >= 1:
			add("quality", "Профиль риск/прибыль", "ok", fmt.Sprintf("Макс. прибыль ≥ макс. убытка (R:R %.2f)", rr), 20, 20)
		case rr >= 0.66:
			add("quality", "Профиль риск/прибыль", "warn", fmt.Sprintf("R:R %.2f ниже единицы", rr), 11, 20)
		default:
			add("quality", "Профиль риск/прибыль", "bad", fmt.Sprintf("R:R %.2f — убыток превышает прибыль", rr), 4, 20)
		}
	default:
		add("quality", "Экономика структуры", "info", "Недостаточно данных плана", 8, 20)
	}

	// 5) Leg liquidity: bid/ask width as % of mid.
	switch {
	case in.LiquidityPct < 0:
		add("liquidity", "Ликвидность ног", "info", "Двусторонние котировки недоступны", 8, 15)
	case in.LiquidityPct <= 10:
		add("liquidity", "Ликвидность ног", "ok", fmt.Sprintf("Спред стаканов ≈%.1f%% от середины", in.LiquidityPct), 15, 15)
	case in.LiquidityPct <= 25:
		add("liquidity", "Ликвидность ног", "warn", fmt.Sprintf("Широкие стаканы (≈%.0f%%) — используйте лимит-заявки", in.LiquidityPct), 9, 15)
	default:
		add("liquidity", "Ликвидность ног", "bad", fmt.Sprintf("Стаканы очень широкие (≈%.0f%%) — вход дорог", in.LiquidityPct), 3, 15)
	}

	adv.Score = score
	if score >= 70 {
		adv.Verdict = "РЕКОМЕНДУЕТСЯ"
	} else if score >= 45 {
		adv.Verdict = "ОСТОРОЖНО"
	} else {
		adv.Verdict = "НЕ РЕКОМЕНДУЕТСЯ"
	}
	return adv
}

// scoreTrendFit fills check #2 (direction vs trend); kept separate to keep
// scoreSpreadAdvice readable.
func scoreTrendFit(in adviceInputs, add func(id, title, status, detail string, earned, weight int)) {
	bullishType := isBullishType(typeOf(in))
	title := "Направление и тренд БА"
	regime := in.TrendRegime
	if regime == "" {
		add("trend", title, "info", "Тренд не определён (нет истории цен)", 12, 25)
		return
	}
	dirWord := "бычья"
	if !bullishType {
		dirWord = "медвежья"
	}
	trendBull := regime == "BULLISH"
	aligned := trendBull == bullishType
	switch regime {
	case "BULLISH", "BEARISH":
		strengthAligned := (regime == "BULLISH" && in.TrendStrength == "rising") ||
			(regime == "BEARISH" && in.TrendStrength == "falling")
		if aligned && strengthAligned {
			add("trend", title, "ok", fmt.Sprintf("%s структура по тренду %s с импульсом", dirWord, regimeLabel(regime)), 25, 25)
		} else if aligned {
			add("trend", title, "ok", fmt.Sprintf("%s структура по тренду %s", dirWord, regimeLabel(regime)), 20, 25)
		} else if !in.IsDebit {
			add("trend", title, "warn", fmt.Sprintf("Против тренда %s: кредит прощает, но короткий страйк под давлением", regimeLabel(regime)), 10, 25)
		} else {
			add("trend", title, "bad", fmt.Sprintf("Дебетовая структура против тренда %s — нужен разворот", regimeLabel(regime)), 4, 25)
		}
	case "SIDEWAYS":
		if !in.IsDebit {
			add("trend", title, "ok", "Боковик: продажа премии вне денег уместна, держите страйки дальше центра", 18, 25)
		} else {
			add("trend", title, "warn", "Боковик: дебетовой структуре нужно движение, которого нет", 6, 25)
		}
	}
}

// typeOf extracts the spread type from inputs without duplicating fields.
func typeOf(in adviceInputs) string { return in.spreadType }

// regimeLabel maps internal regimes to Russian wording used in the UI.
func regimeLabel(r string) string {
	switch r {
	case "BULLISH":
		return "восходящий"
	case "BEARISH":
		return "нисходящий"
	case "SIDEWAYS":
		return "боковик"
	default:
		return "неопределённый"
	}
}

// ---- market data gathering ----

// resolveNearestFuturesCode picks the live contract of this root expiring on or
// after today (used when selectedSeries holds a synthetic expiry code).
func resolveNearestFuturesCode(symbol string) string {
	contracts, err := moexFuturesContracts()
	if err != nil {
		return ""
	}
	today := time.Now().Format("2006-01-02")
	best := ""
	bestDate := ""
	for _, c := range contracts {
		if !strings.HasPrefix(c.Code, symbol) || c.LastDelDate < today {
			continue
		}
		if best == "" || c.LastDelDate < bestDate {
			best, bestDate = c.Code, c.LastDelDate
		}
	}
	return best
}

// stockDailyCloses fetches daily closing prices for a TQBR share from the ISS
// history endpoint (paginated).
func stockDailyCloses(ticker string) ([]float64, error) {
	from := time.Now().AddDate(0, 0, -110).Format("2006-01-02")
	till := time.Now().Format("2006-01-02")
	client := &http.Client{Timeout: 10 * time.Second}
	var out []float64
	for start := 0; start <= 200; start += 100 {
		url := fmt.Sprintf("http://iss.moex.com/iss/history/engines/stock/markets/shares/boards/TQBR/securities/%s.json?iss.meta=off&iss.only=history&history.columns=TRADEDATE,CLOSE&from=%s&till=%s&start=%d",
			ticker, from, till, start)
		resp, err := client.Get(url)
		if err != nil {
			break
		}
		var data struct {
			History struct {
				Data [][]interface{} `json:"data"`
			} `json:"history"`
		}
		err = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if err != nil {
			break
		}
		for _, row := range data.History.Data {
			if len(row) < 2 {
				continue
			}
			cl, _ := row[1].(float64)
			if cl > 0 {
				out = append(out, cl)
			}
		}
		if len(data.History.Data) < 100 {
			break
		}
	}
	return out, nil
}

// underlyingCloses returns ~80 calendar days of daily closes for the spread's
// underlying: stock history for SBER/SBERP, futures candles for FORTS assets.
func underlyingCloses(symbol string) ([]float64, error) {
	if _, isEquity := equityOptions[symbol]; isEquity {
		closes, err := stockDailyCloses(symbol)
		if err != nil || len(closes) == 0 {
			return nil, fmt.Errorf("нет истории цены для %s", symbol)
		}
		return closes, nil
	}
	code := selectedSeriesFor(symbol)
	if code == "" || isSyntheticSeriesCode(code) {
		code = resolveNearestFuturesCode(symbol)
	}
	if code == "" {
		return nil, fmt.Errorf("нет фьючерсной серии для %s", symbol)
	}
	from := time.Now().AddDate(0, 0, -85).Format("2006-01-02")
	till := time.Now().Format("2006-01-02")
	candles, err := fetchFutureHistory(code, from, till)
	if err != nil {
		return nil, err
	}
	closes := make([]float64, 0, len(candles))
	for _, c := range candles {
		if c.Close > 0 {
			closes = append(closes, c.Close)
		}
	}
	if len(closes) == 0 {
		return nil, fmt.Errorf("пустая история фьючерса %s", code)
	}
	return closes, nil
}

// spreadAdviceHandler returns the pre-trade decision panel for a planned
// vertical spread.
// URL: /api/v1/spreads/advice?symbol=Si&type=bull_put&expiry=&qty=1
func spreadAdviceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	spreadType := r.URL.Query().Get("type")
	if spreadType == "" {
		spreadType = "bull_put"
	}
	expiry := r.URL.Query().Get("expiry")
	qty := 1
	if v := r.URL.Query().Get("qty"); v != "" {
		fmt.Sscanf(v, "%d", &qty)
	}

	plan, err := buildVerticalSpread(symbol, spreadType, expiry, qty)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}

	in := adviceInputs{
		IsDebit:        isDebitSpreadType(spreadType),
		DTE:            plan.DaysToExp,
		CreditPerShare: plan.NetCredit,
		Width:          plan.WingWidth,
		MaxProfit:      plan.MaxProfit,
		MaxLoss:        plan.MaxLoss,
		LiquidityPct:   -1,
	}
	in.spreadType = spreadType

	ivRaw := currentATMIVRaw(symbol)
	if ivRaw > 0 {
		in.IvATM = ivRaw
	}
	if rs := ivRankStats(symbol, ivRaw); rs["available"] == true {
		if v, ok := rs["iv_rank"].(float64); ok {
			in.IvRankAvail = true
			in.IvRank = v
		}
	}

	metrics := map[string]float64{"spot": plan.Spot, "dte": float64(plan.DaysToExp)}
	if ivRaw > 0 {
		metrics["atm_iv"] = math.Round(ivRaw*10000) / 100
	}

	if closes, cerr := underlyingCloses(symbol); cerr == nil && len(closes) >= 55 {
		if hv := hvFromCloses(closes[len(closes)-21:], 20); hv > 0 {
			in.Hv20 = hv
			metrics["hv20"] = math.Round(hv*10000) / 100
			if ivRaw > 0 {
				metrics["iv_hv_edge"] = math.Round((ivRaw-hv)*10000) / 100
			}
		}
		ts := computeTrendStats(closes)
		in.TrendRegime = ts.Regime
		in.TrendStrength = ts.Strength
		metrics["sma20"] = math.Round(ts.SMA20*100) / 100
		metrics["sma50"] = math.Round(ts.SMA50*100) / 100
		metrics["rsi14"] = math.Round(ts.RSI14*10) / 10
		metrics["roc10"] = math.Round(ts.ROC10*10000) / 100
		metrics["trend_slope5"] = math.Round(ts.Slope5*10000) / 100
	}

	pcts := make([]float64, 0, len(plan.Legs))
	for _, l := range plan.Legs {
		_, b, o := cachedOptionQuote(l.SecID)
		if b > 0 && o > 0 && o >= b {
			if mid := (b + o) / 2; mid > 0 {
				pcts = append(pcts, (o-b)/mid*100)
			}
		}
	}
	if len(pcts) > 0 {
		s := 0.0
		for _, p := range pcts {
			s += p
		}
		in.LiquidityPct = s / float64(len(pcts))
		metrics["liq_pct"] = math.Round(in.LiquidityPct*10) / 10
	}

	adv := scoreSpreadAdvice(in)
	scoreTrendFit(in, func(id, title, status, detail string, earned, weight int) {
		adv.Checks = append(adv.Checks, adviceCheck{ID: id, Title: title, Status: status, Detail: detail, Earned: earned, Weight: weight})
		adv.Score += earned
	})
	// Recompute the verdict now that all checks are collected.
	adv.Score = 0
	for _, c := range adv.Checks {
		adv.Score += c.Earned
	}
	if adv.Score >= 70 {
		adv.Verdict = "РЕКОМЕНДУЕТСЯ"
	} else if adv.Score >= 45 {
		adv.Verdict = "ОСТОРОЖНО"
	} else {
		adv.Verdict = "НЕ РЕКОМЕНДУЕТСЯ"
	}

	var issues []string
	for _, c := range adv.Checks {
		if c.Status == "bad" {
			issues = append(issues, c.Title+": "+c.Detail)
		}
	}
	for _, c := range adv.Checks {
		if c.Status == "warn" {
			issues = append(issues, c.Title+": "+c.Detail)
		}
	}
	if len(issues) == 0 {
		adv.Summary = "Все проверки пройдены — структура соответствует базе знаний."
	} else {
		adv.Summary = strings.Join(issues, " · ")
	}

	adv.Symbol = symbol
	adv.Type = spreadType
	adv.Expiry = plan.Expiry
	adv.Spot = plan.Spot
	adv.DTE = plan.DaysToExp
	adv.Metrics = metrics
	json.NewEncoder(w).Encode(adv)
}
