package main

// Analytics core, data layer: collects the full market picture per instrument
// (underlying trend context, volatility surface summary, liquidity) and
// generates ranked trade candidates filtered by the KNOWLEDGE.md rules.

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"option-quant-ai/quant"
)

const coreSymbols = "Si,RI,CR,NG,SBER,SBERP"

type coreInstrument struct {
	Symbol       string  `json:"symbol"`
	Spot         float64 `json:"spot"`
	Regime       string  `json:"regime"` // BULLISH / BEARISH / SIDEWAYS / ""
	Strength     string  `json:"strength"`
	SMA20        float64 `json:"sma20"`
	SMA50        float64 `json:"sma50"`
	SMA200       float64 `json:"sma200"`
	AboveSMA200  bool    `json:"above_sma200"`
	RSI14        float64 `json:"rsi14"`
	ADX14        float64 `json:"adx14"` // close-to-close approximation
	IVATM        float64 `json:"iv_atm"`
	IVRank       float64 `json:"iv_rank"`
	HV20         float64 `json:"hv20"`
	Skew25       float64 `json:"skew25"` // put wing IV minus call wing IV, pp
	TermFront    float64 `json:"term_front"`
	TermNext     float64 `json:"term_next"`
	LiquidityPct float64 `json:"liquidity_pct"`
	ExpiryFront  string  `json:"expiry_front"`
	DTEFront     int     `json:"dte_front"`
	Err          string  `json:"err,omitempty"`
	ATR14        float64 `json:"atr14"`  // average true range proxy: avg |Δclose| over 14 sessions
	Volume       int     `json:"volume"` // last trade volume from MOEX ISS
}

type coreCandidate struct {
	Symbol      string   `json:"symbol"`
	Strategy    string   `json:"strategy"`
	DisplayName string   `json:"display_name"`
	Expiry      string   `json:"expiry"`
	DTE         int      `json:"dte"`
	ShortStrike float64  `json:"short_strike"`
	LongStrike  float64  `json:"long_strike"`
	NetCredit   float64  `json:"net_credit"`
	MaxProfit   float64  `json:"max_profit"`
	MaxLoss     float64  `json:"max_loss"`
	Score       int      `json:"score"`
	Reasons     []string `json:"reasons"`
	StopLoss    float64  `json:"stop_loss"`
	TakeProfit  float64  `json:"take_profit"`
	PopProb     int      `json:"pop_prob"` // probability of profit %, from Monte Carlo
}

type coreBrief struct {
	GeneratedAt string             `json:"generated_at"`
	Instruments []coreInstrument   `json:"instruments"`
	Candidates  []coreCandidate    `json:"candidates"`
	Portfolio   map[string]float64 `json:"portfolio"`
	KB          []string           `json:"kb"`
}

var (
	coreBriefMu      sync.Mutex
	coreBriefCache   *coreBrief
	coreBriefCacheAt time.Time
)

func smaNFrom(closes []float64, n int) float64 { return smaN(closes, n) }

// approxADX14 computes a close-to-close directional-movement approximation of
// ADX (no true highs/lows — the ISS close fetch keeps the brief light).
func approxADX14(closes []float64) float64 {
	if len(closes) < 30 {
		return 0
	}
	var plusDM, minusDM, tr []float64
	for i := 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		p, m := math.Max(d, 0), math.Max(-d, 0)
		plusDM = append(plusDM, p)
		minusDM = append(minusDM, m)
		tr = append(tr, math.Abs(d))
	}
	n := 14
	sum := func(a []float64, from int) float64 {
		s := 0.0
		for i := from; i < from+n && i < len(a); i++ {
			s += a[i]
		}
		return s
	}
	sP, sM, sT := sum(plusDM, 1), sum(minusDM, 1), sum(tr, 1)
	if sT == 0 {
		return 0
	}
	dx := math.Abs(sP-sM) / sT * 100
	// Single smoothing pass over the rest — a rough but stable estimate.
	for i := 1 + n; i+n < len(plusDM); i += n {
		sP = sP - sP/float64(n) + sum(plusDM, i)/float64(n)
		sM = sM - sM/float64(n) + sum(minusDM, i)/float64(n)
		sT = sT - sT/float64(n) + sum(tr, i)/float64(n)
		if sT > 0 {
			dx = 0.5*dx + 0.5*(math.Abs(sP-sM)/sT*100)
		}
	}
	return math.Round(dx*10) / 10
}

// computeATR14 computes a simple ATR proxy: average of absolute close changes
// over the last `period` sessions.  Good enough for the core brief because we
// only have closing prices from ISS.
func computeATR14(closes []float64, period int) float64 {
	if len(closes) < period+1 {
		return 0
	}
	var s float64
	for i := len(closes) - period - 1; i < len(closes)-1; i++ {
		s += math.Abs(closes[i+1] - closes[i])
	}
	return math.Round(s/float64(period)*100) / 100
}

// monteCarloPop estimates the probability of profit (PoP) for a vertical spread.
// It simulates terminal underlying prices using a log‑normal (Geometric Brownian)
// model with zero drift, annualised volatility = iv, and dte days to expiration.
// For a credit spread (netCredit > 0) the payoff is:
//
//	profit = netCredit   if finalSpot <= shortStrike
//	         netCredit - (finalSpot - shortStrike)   if finalSpot > shortStrike && finalSpot <= longStrike
//	         -maxLoss   if finalSpot > longStrike
//
// For a debit spread (netCredit <= 0) the rule is analogous with the signs flipped.
// The function returns the proportion of simulations where profit > 0.
// mcSeed pins the Monte-Carlo RNG so PoP — and therefore the candidate score
// and ranking — is reproducible scan after scan. A fixed stream is fine: each
// call gets a fresh RNG, and different inputs still yield different PoPs.
const mcSeed = 42

func monteCarloPop(netCredit, maxLoss, spot, ivAnnual, shortStrike, longStrike float64, dte int, simulations int) int {
	if dte <= 0 || simulations <= 0 {
		return 0
	}
	T := float64(dte) / 365.0
	vol := ivAnnual * math.Sqrt(T)
	rng := rand.New(rand.NewSource(mcSeed))
	// Vertical-spread P&L at expiry from aggregates only. Orientation comes
	// from the strikes: bull spreads (short above long) profit when the
	// underlying ends HIGH, bear spreads (short below long) when it ends
	// LOW. K_hi/K_lo bracket the slope zone; outside it the position pays
	// the capped max profit or max loss.
	kHi := math.Max(shortStrike, longStrike)
	kLo := math.Min(shortStrike, longStrike)
	bull := shortStrike > longStrike
	profitable := 0
	for i := 0; i < simulations; i++ {
		// terminal price under GBM with zero drift
		z := rng.NormFloat64()
		finalSpot := spot * math.Exp(-0.5*ivAnnual*ivAnnual*T+vol*z)
		var profit float64
		if netCredit > 0 {
			// Credit spread: capped upside is the credit itself.
			switch {
			case bull && finalSpot >= kHi:
				profit = netCredit
			case bull && finalSpot > kLo:
				profit = netCredit - (kHi - finalSpot)
			case !bull && finalSpot <= kLo:
				profit = netCredit
			case !bull && finalSpot < kHi:
				profit = netCredit - (finalSpot - kLo)
			default:
				profit = -maxLoss
			}
		} else {
			// Debit spread: capped upside is wing minus the debit paid.
			debit := -netCredit
			maxProfit := (kHi - kLo) - debit
			switch {
			case bull && finalSpot >= kHi:
				profit = maxProfit
			case bull && finalSpot > kLo:
				profit = maxProfit - (kHi - finalSpot)
			case !bull && finalSpot <= kLo:
				profit = maxProfit
			case !bull && finalSpot < kHi:
				profit = maxProfit - (finalSpot - kLo)
			default:
				profit = -debit
			}
		}
		if profit > 0 {
			profitable++
		}
	}
	return profitable * 100 / simulations // return percent
}

// popScoreAdjust converts a Monte-Carlo PoP (0–100) into a score adjustment
// around the neutral 50%: high chance of profit adds up to +8, low chance
// subtracts up to −8. It also returns a reason line for the visible bands
// (empty for the neutral 40–54 zone). Pure — covered by unit tests.
func popScoreAdjust(pop int) (int, string) {
	switch {
	case pop >= 70:
		return 8, fmt.Sprintf("Шанс прибыли %d%% — высокий", pop)
	case pop >= 55:
		return 4, ""
	case pop >= 40:
		return 0, ""
	case pop >= 30:
		return -4, ""
	default:
		return -8, fmt.Sprintf("✗ Шанс прибыли %d%% — низкий", pop)
	}
}

// collectCoreInstrument builds the market brief for one instrument.
func collectCoreInstrument(symbol string) coreInstrument {
	in := coreInstrument{Symbol: symbol}
	closes, cerr := underlyingCloses(symbol)
	if cerr == nil && len(closes) >= 55 {
		ts := computeTrendStats(closes)
		in.Spot = ts.Close
		in.Regime = ts.Regime
		in.Strength = ts.Strength
		in.SMA20 = math.Round(ts.SMA20*100) / 100
		in.SMA50 = math.Round(ts.SMA50*100) / 100
		in.RSI14 = math.Round(ts.RSI14*10) / 10
		in.ADX14 = approxADX14(closes)
		if s200 := smaNFrom(closes, 200); s200 > 0 {
			in.SMA200 = math.Round(s200*100) / 100
			in.AboveSMA200 = ts.Close > s200
		}
		if hv := hvFromCloses(closes[len(closes)-21:], 20); hv > 0 {
			in.HV20 = math.Round(hv*10000) / 100
		}
	} else {
		in.Err = "мало истории цен"
		if s, err := getSpotPrice(symbol); err == nil {
			in.Spot = s
		}
	}
	// ATR14 proxy (average |Δclose| over last 14 sessions).
	if len(closes) >= 21 {
		in.ATR14 = computeATR14(closes, 14)
	}

	// Volume from MOEX ISS (last trade volume of the front-month future).
	if v, err := moexISSVolume(symbol); err == nil {
		in.Volume = v
	}

	// Option surface: two nearest live expiries.
	series := optionSeriesForSymbol(symbol)
	sort.Slice(series, func(i, j int) bool { return series[i].LastDelDate < series[j].LastDelDate })
	today := time.Now().Format("2006-01-02")
	var expFront, expNext string
	for _, s := range series {
		if s.LastDelDate >= today {
			if expFront == "" {
				expFront = s.LastDelDate
			} else {
				expNext = s.LastDelDate
				break
			}
		}
	}
	in.ExpiryFront = expFront
	in.DTEFront = dteInDays(expFront, time.Now())

	atmIV := func(expiry string) float64 {
		if expiry == "" {
			return 0
		}
		chain := moexOptionsForAsset(symbol, expiry)
		if len(chain) == 0 {
			return 0
		}
		strikes, _, err := optionChainFor(symbol, expiry)
		if err != nil {
			return 0
		}
		k := nearestStrikeFromStrikes(strikes, in.Spot)
		for i := range chain {
			if chain[i].Strike == k {
				t := float64(dteInDays(expiry, time.Now())) / 365.0
				if t <= 0 {
					t = 1.0 / 365.0
				}
				q, err2 := moexOptionQuoteEx(chain[i].SecID)
				if err2 != nil || q.Price <= 0 {
					return 0
				}
				iv := quantIV(chain[i].IsCall, q.Price, in.Spot, k, t)
				return math.Round(iv*10000) / 100
			}
		}
		return 0
	}
	in.IVATM = atmIV(expFront)
	if iv2 := atmIV(expNext); iv2 > 0 {
		in.TermNext = iv2
		in.TermFront = in.IVATM
	}
	// IV Rank via the accumulated history.
	if rs := ivRankStats(symbol, currentATMIVRaw(symbol)); rs["available"] == true {
		if v, ok := rs["iv_rank"].(float64); ok {
			in.IVRank = v
		}
	}
	// Skew proxy: put wing (−3%) vs call wing (+3%) IV on the front expiry.
	if expFront != "" && in.Spot > 0 {
		chain := moexOptionsForAsset(symbol, expFront)
		t := float64(dteInDays(expFront, time.Now())) / 365.0
		if t <= 0 {
			t = 1.0 / 365.0
		}
		ivAt := func(strike float64, isCall bool) float64 {
			for i := range chain {
				if chain[i].Strike == strike && chain[i].IsCall == isCall {
					if q, err := moexOptionQuoteEx(chain[i].SecID); err == nil && q.Price > 0 {
						return quantIV(chain[i].IsCall, q.Price, in.Spot, strike, t)
					}
				}
			}
			return 0
		}
		strikes, _, err := optionChainFor(symbol, expFront)
		if err == nil {
			ks := strikesBelow(strikes, in.Spot*0.97, 1)
			kc := strikesAbove(strikes, in.Spot*1.03, 1)
			if len(ks) == 1 && len(kc) == 1 {
				piv, civ := ivAt(ks[0], false), ivAt(kc[0], true)
				if piv > 0 && civ > 0 {
					in.Skew25 = math.Round((piv-civ)*1000) / 10
				}
			}
		}
	}
	// Liquidity: ATM bid/ask width on the front expiry.
	if expFront != "" {
		chain := moexOptionsForAsset(symbol, expFront)
		for i := range chain {
			if chain[i].Strike == nearestStrike(chain, in.Spot) {
				if q, err := moexOptionQuoteEx(chain[i].SecID); err == nil && q.Bid > 0 && q.Offer >= q.Bid {
					in.LiquidityPct = math.Round((q.Offer-q.Bid)/((q.Bid+q.Offer)/2)*1000) / 10
				}
				break
			}
		}
	}
	return in
}

func quantIV(isCall bool, price, S, K, t float64) float64 {
	iv := quant.ImpliedVolatility(isCall, price, S, K, t, 0.16)
	if iv < 0.02 {
		iv = 0.30
	}
	if iv > 3 {
		iv = 3
	}
	return iv
}

func quantGetActive() []quant.Position { return quant.GetActivePositions() }

// collectCoreBrief gathers the full picture; cached for 5 minutes.
func collectCoreBrief(force bool) *coreBrief {
	coreBriefMu.Lock()
	if !force && coreBriefCache != nil && time.Since(coreBriefCacheAt) < 5*time.Minute {
		b := coreBriefCache
		coreBriefMu.Unlock()
		return b
	}
	coreBriefMu.Unlock()

	symbols := []string{"Si", "RI", "CR", "NG", "SBER", "SBERP"}
	var wg sync.WaitGroup
	instruments := make([]coreInstrument, len(symbols))
	for i, s := range symbols {
		wg.Add(1)
		go func(idx int, sym string) {
			defer wg.Done()
			instruments[idx] = collectCoreInstrument(sym)
		}(i, s)
	}
	wg.Wait()

	b := &coreBrief{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Instruments: instruments,
		Candidates:  coreBuildCandidates(instruments),
	}
	pos := quantActiveSummary()
	b.Portfolio = pos
	b.KB = []string{
		"Вход: 14–45 DTE, шорт-страйк 16–25Δ, кредит ≥ 1/3 ширины крыла, стаканы ≤ 10% от mid.",
		"T/P: 50–75% макс. прибыли. Стоп: убыток 1.5–2× кредита (0.6–0.8 макс. убытка).",
		"Ролл только за нет-кредит; дебетовый ролл запрещён — держим до экспирации.",
		"TPR по 1σ: против позиции → новый прогноз: рост→лестница, боковик→ratio, падение→ATM put.",
		"Не держать короткие путы SBER через дивидендный гэп; короткая гамма у экспирации — time-stop.",
		"Используйте ATR14 (средний диапазон закрытий за 14 сессий) как ориентир стопа: стоп umístьте на 1–1.5× ATR14 от входа, чтобы отсечь шум.",
	}

	coreBriefMu.Lock()
	coreBriefCache = b
	coreBriefCacheAt = time.Now()
	coreBriefMu.Unlock()
	return b
}

func quantActiveSummary() map[string]float64 {
	positions := quantGetActive()
	margin, unreal, delta := 0.0, 0.0, 0.0
	for _, p := range positions {
		margin += p.Margin
		unreal += p.PnL
		delta += p.Delta
	}
	return map[string]float64{
		"open_positions": float64(len(positions)),
		"margin":         math.Round(margin*100) / 100,
		"unrealized_pnl": math.Round(unreal*100) / 100,
		"net_delta":      math.Round(delta*100) / 100,
	}
}

// coreBuildCandidates scans verticals (and iron condors in flat+expensive
// regimes) per instrument, scores them with the pre-trade advice model and
// returns the best few.
func coreBuildCandidates(instruments []coreInstrument) []coreCandidate {
	out := []coreCandidate{}
	open := openSpreads()
	for _, in := range instruments {
		if in.ExpiryFront == "" || in.Spot <= 0 {
			continue
		}
		expiry := in.ExpiryFront
		if in.DTEFront < 7 || in.DTEFront > 45 {
			// Prefer a series inside the KB window when the front one is out.
			series := optionSeriesForSymbol(in.Symbol)
			sort.Slice(series, func(i, j int) bool { return series[i].LastDelDate < series[j].LastDelDate })
			today := time.Now().Format("2006-01-02")
			for _, s := range series {
				d := dteInDays(s.LastDelDate, time.Now())
				if s.LastDelDate >= today && d >= 14 && d <= 45 {
					expiry = s.LastDelDate
					break
				}
			}
		}
		ivExpensive := in.IVATM > 0 && in.HV20 > 0 && (in.IVATM-in.HV20) >= 5
		ivCheap := in.IVATM > 0 && in.HV20 > 0 && (in.IVATM-in.HV20) <= -5
		var types []string
		switch in.Regime {
		case "BULLISH":
			if ivExpensive || in.IVRank >= 40 {
				types = append(types, "bull_put")
			}
			if ivCheap || in.IVRank < 30 {
				types = append(types, "bull_call")
			}
		case "BEARISH":
			if ivExpensive || in.IVRank >= 40 {
				types = append(types, "bear_call")
			}
			if ivCheap || in.IVRank < 30 {
				types = append(types, "bear_put")
			}
		case "SIDEWAYS":
			if ivExpensive || in.IVRank >= 50 {
				types = append(types, "bull_put", "bear_call")
			}
		}
		for _, ty := range types {
			plan, err := buildVerticalSpread(in.Symbol, ty, expiry, 1)
			if err != nil {
				continue
			}
			// Drop constructions with impossible economics (credit above the
			// wing, debit above the max payout): the leg marks are broken and
			// the "candidate" would be garbage in the table, Telegram alerts
			// and paper auto-entry alike.
			if !planEconomicsSane(plan) {
				continue
			}
			// Do not recommend a construction whose twin (same symbol, type and
			// strike pair) is already open — reopening the same position just
			// multiplies exposure on an existing trade.
			if spreadAlreadyOpen(plan, open) {
				continue
			}
			in2 := in
			in2.ExpiryFront = expiry
			cand := candidateFromPlan(plan, in2)
			if cand.Score < 45 {
				continue
			}
			// Risk‑adjusted score: penalise candidates whose max loss is large
			// relative to the recent ATR14 (proxy for expected daily move in %).
			if in.ATR14 > 0 {
				riskRatio := cand.MaxLoss / (in.ATR14 * 100) // ATR14 is percent, MaxLoss is rubles per contract; scale factor 100 normalises.
				if riskRatio > 3.0 {
					cand.Score = int(float64(cand.Score) * 0.7) // down‑weight
				}
				if riskRatio < 1.0 {
					cand.Score = int(float64(cand.Score) * 1.1) // small‑risk bonus
				}
			}
			// Apply quant weight to the candidate score
			if coreSet.QuantWeight > 0 {
				cand.Score = int(float64(cand.Score) * coreSet.QuantWeight)
			}
			out = append(out, cand)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// spreadAlreadyOpen reports whether an identical construction (same symbol,
// type and strike pair) is already OPEN. The Core must not re-recommend a
// spread the trader already holds — reopening it would silently multiply the
// same directional/short-gamma risk instead of opening a new trade.
func spreadAlreadyOpen(plan *spreadPlan, open []spreadRecord) bool {
	for _, s := range open {
		if s.Symbol != plan.Symbol || s.Type != plan.Type {
			continue
		}
		if strikesEqual(s.ShortStrike, plan.ShortStrike) && strikesEqual(s.LongStrike, plan.LongStrike) {
			return true
		}
	}
	return false
}

// strikesEqual compares strikes with the tolerance used across the chain
// lookup (half a step), so AL2 vs AL2.5 style series map to the same strike.
func strikesEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.5
}

func candidateFromPlan(plan *spreadPlan, in coreInstrument) coreCandidate {
	in2 := in
	in2.ExpiryFront = plan.Expiry
	in2.DTEFront = plan.DaysToExp
	advIn := adviceInputs{
		spreadType:     plan.Type,
		IsDebit:        plan.IsDebit,
		DTE:            plan.DaysToExp,
		CreditPerShare: plan.NetCredit,
		Width:          plan.WingWidth,
		MaxProfit:      plan.MaxProfit,
		MaxLoss:        plan.MaxLoss,
		IvATM:          in.IVATM / 100,
		Hv20:           in.HV20 / 100,
		TrendRegime:    in.Regime,
		TrendStrength:  in.Strength,
		LiquidityPct:   in.LiquidityPct,
	}
	adv := scoreSpreadAdvice(advIn)
	scoreTrendFit(advIn, func(id, title, status, detail string, earned, weight int) {
		adv.Checks = append(adv.Checks, adviceCheck{ID: id, Title: title, Status: status, Detail: detail, Earned: earned, Weight: weight})
	})
	total := 0
	reasons := []string{}
	for _, c := range adv.Checks {
		total += c.Earned
		if c.Status == "ok" {
			reasons = append(reasons, c.Title+": "+c.Detail)
		} else if c.Status == "bad" {
			reasons = append(reasons, "✗ "+c.Title+": "+c.Detail)
		}
	}
	// Monte Carlo probability of profit (PoP).
	var pop int
	popComputed := plan.DaysToExp > 0 && plan.NetCredit != 0
	if popComputed {
		ivAnnual := in.IVATM / 100.0 // IVATM is already a percent; convert to fraction
		if ivAnnual <= 0 {
			ivAnnual = in.HV20 / 100.0
		}
		pop = monteCarloPop(plan.NetCredit, plan.MaxLoss, in.Spot, ivAnnual,
			plan.ShortStrike, plan.LongStrike, plan.DaysToExp, 2000)
	}
	// PoP weight: the Monte-Carlo chance of profit nudges the score around
	// the neutral 50% (±8 points). High PoP is listed in the reasons, low
	// PoP is flagged — so the number shown in the table actually moves the
	// ranking instead of being decoration.
	if popComputed {
		adj, line := popScoreAdjust(pop)
		total += adj
		if line != "" {
			reasons = append(reasons, line)
		}
		if total < 0 {
			total = 0
		}
	}
	// Dynamic stop / take‑profit based on ATR14 and Monte‑Carlo PoP.
	stopPrice := 0.0
	takeProfit := 0.0
	if in.ATR14 > 0 {
		// base levels (from earlier implementation)
		baseStop := plan.NetCredit - 1.3*in.ATR14
		baseTP := plan.NetCredit + 0.5*plan.MaxProfit

		// adjust according to PoP
		if pop >= 70 {
			// high confidence → tighten stop, raise target
			stopPrice = plan.NetCredit - 0.9*in.ATR14
			takeProfit = plan.NetCredit + 0.7*plan.MaxProfit
		} else if pop < 30 {
			// low confidence → widen stop, lower target
			stopPrice = plan.NetCredit - 1.5*in.ATR14
			takeProfit = plan.NetCredit + 0.3*plan.MaxProfit
		} else {
			// medium → use base
			stopPrice = baseStop
			takeProfit = baseTP
		}
		if stopPrice < 0 {
			stopPrice = 0
		}
	}
	// Calendar event warning: penalise candidates whose expiry falls near
	// a known dividend or earnings date.
	if warn, detail := calendarWarning(plan.Symbol, plan.Expiry); warn {
		total -= 10
		reasons = append(reasons, "⚠ Календарь: экспирация рядом с событием ("+detail+")")
	}

	return coreCandidate{
		Symbol: plan.Symbol, Strategy: plan.Type, DisplayName: plan.DisplayName,
		Expiry: plan.Expiry, DTE: plan.DaysToExp,
		ShortStrike: plan.ShortStrike, LongStrike: plan.LongStrike,
		NetCredit: plan.NetCredit, MaxProfit: plan.MaxProfit, MaxLoss: plan.MaxLoss,
		Score: total, Reasons: reasons,
		StopLoss: stopPrice, TakeProfit: takeProfit,
		PopProb: pop,
	}
}
