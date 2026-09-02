package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"option-quant-ai/quant"
)

// errRollDebit means the next series cannot be opened for a net credit. Per the
// management rules (KNOWLEDGE.md §1.2В) the position must then be held until
// expiry instead of being rolled into a worse one.
var errRollDebit = errors.New("новая серия открывается только за дебет")

// spreadRules are the user-configurable management rules for an open vertical
// spread. Zero values disable the corresponding automatic action.
type spreadRules struct {
	// AutoRollDTE triggers a roll when days-to-expiry drop to this value or
	// below (0 disables).
	AutoRollDTE int `json:"auto_roll_dte"`
	// RollCreditPct triggers a roll when the fraction of max profit already
	// captured reaches this value (0.85 = 85% of the credit collected).
	// Applies to credit spreads (max_profit = net credit).
	RollCreditPct float64 `json:"roll_credit_pct"`
	// RollStrikeRiskPct triggers a roll when the spot is within this fraction
	// of the short strike (e.g. 0.02 = price within 2% of the sold strike).
	RollStrikeRiskPct float64 `json:"roll_strike_risk_pct"`
	// StopLossPct closes the whole spread when the drawdown reaches this
	// fraction of the maximum possible loss (0 disables). 0.75 approximates
	// the KNOWLEDGE.md §1.4 loss cap of ~1.5x credit for a one-third-width
	// credit spread.
	StopLossPct float64 `json:"stop_loss_pct"`
	// AutoHedge enables automatic delta hedging of the linked position.
	AutoHedge bool `json:"auto_hedge"`
	// MaxHedgeDelta is the |net delta| threshold above which a hedge order is
	// placed (0 = hedge any non-zero delta).
	MaxHedgeDelta float64 `json:"max_hedge_delta"`
	// Live makes automatic roll/hedge place real Alor orders instead of paper.
	Live bool `json:"live"`
	// State machine rules (vertical spread management spec, KNOWLEDGE.md §5).
	ProfitTargetPct float64 `json:"profit_target_pct"`
	ProfitAction    string  `json:"profit_action"` // CLOSE | ROLL | CONDOR
	TPRMode         string  `json:"tpr_mode"`      // OFF | ONE_DAY_SIGMA | MAX_LOSS
	TPRSigmaMult    float64 `json:"tpr_sigma_mult"`
	SigmaAnnual     float64 `json:"sigma_annual"`
	RollAlpha       float64 `json:"roll_alpha"`
	AllowUndefined  bool    `json:"allow_undefined_risk"`
	ViewOverride    string  `json:"view_override"` // BULLISH | SIDEWAYS | BEARISH
}

// managerRun is the snapshot of one auto-management pass for a spread.
type managerRun struct {
	SpreadID    string   `json:"spread_id"`
	CheckedAt   string   `json:"checked_at"`
	DTE         int      `json:"dte"`
	NetDelta    float64  `json:"net_delta"`
	Pnl         float64  `json:"pnl"`
	MaxProfit   float64  `json:"max_profit"`
	CapturedPct float64  `json:"captured_pct"`
	Action      string   `json:"action"` // ROLL / HEDGE / CLOSE / REVIEW / reconstruction actions
	Detail      string   `json:"detail"`
	Orders      []string `json:"orders,omitempty"`
	Live        bool     `json:"live"`
}

var (
	spreadManagerMu    sync.Mutex
	spreadManagerLog   []managerRun
	spreadManagerOn    bool
	spreadManagerStart time.Time
	spreadManagerRuns  int
)

// spreadManagerEnabled reports whether the background manager goroutine is on.
func spreadManagerEnabled() bool {
	spreadManagerMu.Lock()
	defer spreadManagerMu.Unlock()
	return spreadManagerOn
}

// startSpreadManager launches the background goroutine that periodically
// evaluates open vertical spreads against their management rules.
func startSpreadManager() {
	spreadManagerMu.Lock()
	if spreadManagerOn {
		spreadManagerMu.Unlock()
		return
	}
	spreadManagerOn = true
	spreadManagerStart = time.Now()
	spreadManagerMu.Unlock()

	go func() {
		for {
			time.Sleep(60 * time.Second)
			runSpreadManagerPass()
		}
	}()
}

// runSpreadManagerPass evaluates all OPEN live spreads once (paper spreads from
// the Core autoscan, Live=false, are left untouched). Actions (roll/hedge) are
// executed only when a rule fires; results are logged for the UI.
func runSpreadManagerPass() {
	runs := []managerRun{}
	for _, s := range openSpreads() {
		if !s.Live {
			continue
		}
		r := evaluateSpread(s)
		if r.Action != "NONE" {
			execSpreadAction(&s, &r)
		}
		runs = append(runs, r)
	}

	spreadManagerMu.Lock()
	spreadManagerLog = runs
	spreadManagerRuns++
	spreadManagerMu.Unlock()
}

// evaluateSpread gathers live telemetry for a spread (position PnL/delta,
// days to expiry, spot and an ATM IV estimate for the sigma-based TPR) and
// delegates the decision to decideSpreadAction.
func evaluateSpread(s spreadRecord) managerRun {
	dte := dteInDays(s.Expiry, time.Now())
	netDelta, pnl := 0.0, 0.0
	ivSum, ivN := 0.0, 0

	spotOK, spot := false, 0.0
	if v, err := getSpotPrice(s.Symbol); err == nil && v > 0 {
		spot, spotOK = v, true
	}

	positions := quant.GetActivePositions()
	for i := range positions {
		if positions[i].ID == s.PositionID {
			p := &positions[i]
			repricePosition(p)
			quant.SavePosition(*p)
			netDelta = p.Delta
			pnl = p.PnL
			if spotOK {
				tYears := float64(dte) / 365.0
				if tYears <= 0 {
					tYears = 30.0 / 365.0
				}
				for _, l := range p.Legs {
					if l.Kind != "OPTION" || l.Strike <= 0 || l.CurrentPrice <= 0 {
						continue
					}
					if iv := quant.ImpliedVolatility(l.IsCall, l.CurrentPrice, spot, l.Strike, tYears, 0.16); iv > 0 {
						ivSum += iv
						ivN++
					}
				}
			}
			break
		}
	}
	ivATM := 0.30
	if ivN > 0 {
		ivATM = ivSum / float64(ivN)
	}

	run := decideSpreadAction(s, dte, netDelta, pnl, spot, ivATM, spotOK)
	run.CheckedAt = time.Now().Format(time.RFC3339)
	return run
}

// isBullishType reports whether the spread profits when the underlying rises.
func isBullishType(t string) bool {
	return t == "bull_call" || t == "bull_put"
}

// decideSpreadAction implements the vertical-spread management state machine
// (KNOWLEDGE.md §5). Priority: survival (time stop, stop-loss) → T/P → TPR →
// legacy roll triggers → delta hedge. Reconstruction actions fire only for
// VERTICAL state with an explicit market view; without a view the manager
// raises REVIEW and waits for the user's decision.
func decideSpreadAction(s spreadRecord, dte int, netDelta, pnl, spot, ivATM float64, spotOK bool) managerRun {
	units := float64(s.Qty)
	if units < 1 {
		units = 1
	}
	scale := contractMultiplier(s.Symbol) * units
	maxProfit := s.MaxProfit * scale
	maxLoss := s.MaxLoss * scale

	run := managerRun{
		SpreadID:  s.ID,
		DTE:       dte,
		NetDelta:  math.Round(netDelta*100) / 100,
		Pnl:       math.Round(pnl*100) / 100,
		MaxProfit: math.Round(maxProfit*100) / 100,
	}
	if maxProfit > 0 {
		run.CapturedPct = math.Round(pnl/maxProfit*1000) / 1000
	}

	isVertical := s.State == "" || s.State == "VERTICAL"

	// Time stop: reconstructed short-gamma structures must not hang into
	// expiry (spec §15).
	if !isVertical && s.AutoRollDTE > 0 && dte <= s.AutoRollDTE {
		run.Action = "CLOSE"
		run.Detail = fmt.Sprintf("Time stop (%s): DTE %d ≤ %d — закрываем реконструкцию", s.State, dte, s.AutoRollDTE)
		return run
	}

	// State branches for reconstructions (spec §8/§10).
	switch s.State {
	case "LADDER":
		if spotOK && s.TPR2 > 0 && spot >= s.TPR2 {
			run.Action = "BUYBACK_FAR_SHORT"
			run.Detail = fmt.Sprintf("Ladder: спот %.2f ≥ TPR2 %.2f — откупаем дальний шорт, снова vertical", spot, s.TPR2)
			return run
		}
		if spotOK && s.TPR1 > 0 && spot <= s.TPR1 {
			run.Action = "SHIFT_LEFT"
			run.Detail = fmt.Sprintf("Ladder: спот %.2f ≤ TPR1 %.2f — сдвигаем лестницу влево", spot, s.TPR1)
			return run
		}
	case "RATIO":
		if spotOK && s.TPR2 > 0 && spot >= s.TPR2 {
			run.Action = "BUYBACK_EXTRA"
			run.Detail = fmt.Sprintf("Ratio: спот %.2f ≥ TPR2 %.2f — откупаем доп. коллы, снова vertical", spot, s.TPR2)
			return run
		}
	}

	// 1) Stop-loss / MAX_LOSS mode of the spec (§3.2).
	if s.StopLossPct > 0 && maxLoss > 0 && pnl <= -s.StopLossPct*maxLoss {
		run.Action = "CLOSE"
		run.Detail = fmt.Sprintf("Убыток %.0f ₽ достиг порога %.0f%% макс. убытка (%.0f ₽)", pnl, s.StopLossPct*100, maxLoss)
		return run
	}

	// 2) Profit target T/P (spec §5): close by default, optional profit-roll
	// or condor conversion.
	if isVertical && s.ProfitTargetPct > 0 && run.CapturedPct >= s.ProfitTargetPct {
		switch strings.ToUpper(s.ProfitAction) {
		case "ROLL":
			run.Action = "ROLL_PROFIT"
			run.Detail = fmt.Sprintf("T/P: собрано %.0f%% макс. прибыли — ролл на часть прибыли (α=%.2f)", run.CapturedPct*100, s.RollAlpha)
		case "CONDOR":
			run.Action = "CONVERT_CONDOR"
			run.Detail = fmt.Sprintf("T/P: собрано %.0f%% — добавляем медвежье крыло (кондор)", run.CapturedPct*100)
		default:
			run.Action = "CLOSE"
			run.Detail = fmt.Sprintf("T/P: собрано %.0f%% макс. прибыли (цель %.0f%%)", run.CapturedPct*100, s.ProfitTargetPct*100)
		}
		return run
	}

	// 3) TPR by one-day sigma (spec §3.2/§6): adverse move ≥ k·σ/√252 from
	// the entry spot. Reconstruction requires an explicit view; otherwise the
	// manager only raises REVIEW with the decision-tree recommendation.
	if isVertical && s.TPRMode == "ONE_DAY_SIGMA" && spotOK && s.EntrySpot > 0 {
		sigma := s.SigmaAnnual
		if sigma <= 0 {
			sigma = ivATM
		}
		if sigma <= 0 {
			sigma = 0.30
		}
		k := s.TPRSigmaMult
		if k <= 0 {
			k = 1
		}
		move := (spot - s.EntrySpot) / s.EntrySpot
		adverse := move
		if isBullishType(s.Type) {
			adverse = -move
		}
		if adverse >= k*sigma/math.Sqrt(252) {
			switch strings.ToUpper(s.ViewOverride) {
			case "BULLISH":
				run.Action = "CONVERT_LADDER"
				run.Detail = fmt.Sprintf("TPR (−%.1f%% ≥ %.0fσ): прогноз рост — строим лестницу", adverse*100, k)
			case "SIDEWAYS":
				run.Action = "CONVERT_RATIO"
				run.Detail = fmt.Sprintf("TPR (−%.1f%% ≥ %.0fσ): прогноз боковик — ratio/front spread", adverse*100, k)
			case "BEARISH":
				run.Action = "ADD_ATM_PUT"
				run.Detail = fmt.Sprintf("TPR (−%.1f%% ≥ %.0fσ): прогноз падение — покупаем ATM put", adverse*100, k)
			default:
				run.Action = "REVIEW"
				run.Detail = fmt.Sprintf("TPR: движение %.1f%% (≥%.0fσ дневной σ=%.0f%%). Задайте прогноз: BULLISH→лестница, SIDEWAYS→ratio, BEARISH→put", adverse*100, k, sigma*100)
			}
			return run
		}
	}

	// 4) DTE roll trigger.
	if s.AutoRollDTE > 0 && dte <= s.AutoRollDTE {
		run.Action = "ROLL"
		run.Detail = fmt.Sprintf("DTE %d ≤ %d — экспирация близко", dte, s.AutoRollDTE)
		return run
	}

	// 5) Captured-credit trigger (credit spreads only).
	if s.RollCreditPct > 0 && !isDebitSpreadType(s.Type) && run.CapturedPct >= s.RollCreditPct {
		run.Action = "ROLL"
		run.Detail = fmt.Sprintf("Собрано %.0f%% кредита (≥ %.0f%%)", run.CapturedPct*100, s.RollCreditPct*100)
		return run
	}

	// 6) Short-strike proximity trigger.
	if s.RollStrikeRiskPct > 0 && spotOK && s.ShortStrike > 0 {
		dist := math.Abs(spot-s.ShortStrike) / s.ShortStrike
		if dist <= s.RollStrikeRiskPct {
			run.Action = "ROLL"
			run.Detail = fmt.Sprintf("Цена %.2f подошла к стрику %.2f (дист. %.2f%%)", spot, s.ShortStrike, dist*100)
			return run
		}
	}

	// 7) Delta hedge trigger.
	if s.AutoHedge {
		threshold := s.MaxHedgeDelta
		if threshold <= 0 {
			threshold = 1.0
		}
		if math.Abs(run.NetDelta) > threshold {
			run.Action = "HEDGE"
			run.Detail = fmt.Sprintf("|Δ| %.2f > порог %.2f", run.NetDelta, threshold)
			return run
		}
	}

	run.Action = "NONE"
	return run
}

// ---- Reconstruction executors (vertical-spread management spec §8–§11) ----

// legQuote returns a working price for a chain contract: last trade, else
// bid/ask mid, else previous close.
func legQuote(o *optionContract) float64 {
	l, b, a := cachedOptionQuote(o.SecID)
	if l > 0 {
		return l
	}
	if b > 0 && a > 0 {
		return (b + a) / 2
	}
	return o.PrevPrice
}

// appendOptionLeg appends an option leg at the live price and updates the
// position margin (short GO when selling, premium estimate when buying).
func appendOptionLeg(p *quant.Position, o *optionContract, side string, qty int) (string, error) {
	price := legQuote(o)
	if price <= 0 {
		return "", fmt.Errorf("нет котировки для %s", o.SecID)
	}
	kind := "CALL"
	if !o.IsCall {
		kind = "PUT"
	}
	p.Legs = append(p.Legs, quant.PositionLeg{
		SecID:        o.SecID,
		Symbol:       p.Symbol,
		Kind:         "OPTION",
		Side:         side,
		Quantity:     qty,
		Strike:       o.Strike,
		IsCall:       o.IsCall,
		EntryPrice:   price,
		CurrentPrice: price,
	})
	if side == "SELL" && o.IMNP > 0 {
		p.Margin += o.IMNP * float64(qty)
	} else if side == "BUY" {
		p.Margin += price * contractMultiplier(p.Symbol) * float64(qty)
	}
	return fmt.Sprintf("%s %d %s %.0f @ %.2f", side, qty, kind, o.Strike, price), nil
}

// reduceLegs closes up to qty contracts across legs matching strike/side and
// drops emptied legs. Returns the number of contracts actually closed.
func reduceLegs(p *quant.Position, isCall bool, strike float64, side string, qty int) int {
	remaining := qty
	out := p.Legs[:0]
	for _, l := range p.Legs {
		if remaining > 0 && l.Kind == "OPTION" && l.IsCall == isCall && l.Strike == strike && l.Side == side {
			take := min(l.Quantity, remaining)
			l.Quantity -= take
			remaining -= take
			if l.Quantity == 0 {
				continue
			}
		}
		out = append(out, l)
	}
	p.Legs = out
	return qty - remaining
}

func strikesAbove(strikes []float64, from float64, n int) []float64 {
	var out []float64
	for _, s := range strikes {
		if s > from {
			out = append(out, s)
			if len(out) == n {
				break
			}
		}
	}
	return out
}

func strikesBelow(strikes []float64, from float64, n int) []float64 {
	var out []float64
	for i := len(strikes) - 1; i >= 0; i-- {
		if strikes[i] < from {
			out = append(out, strikes[i])
			if len(out) == n {
				break
			}
		}
	}
	return out
}

func strikesLessThan(strikes []float64, limit float64) []float64 {
	var out []float64
	for _, s := range strikes {
		if s < limit {
			out = append(out, s)
		}
	}
	return out
}

// persistReconstruction reprices, saves the position and records the new state.
func persistReconstruction(s *spreadRecord, p *quant.Position, state string, tpr1, tpr2 float64) {
	repricePosition(p)
	quant.SavePosition(*p)
	s.State = state
	if state == "VERTICAL" || tpr1 > 0 || tpr2 > 0 {
		s.TPR1, s.TPR2 = tpr1, tpr2
	}
	saveSpreadRecord(*s)
}

// recomputeVerticalEcon refreshes per-share economics once the structure is a
// plain vertical again.
func recomputeVerticalEcon(s *spreadRecord, p *quant.Position) {
	net := 0.0
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, l := range p.Legs {
		if l.Kind != "OPTION" {
			continue
		}
		dir := 1.0
		if l.Side == "SELL" {
			dir = -1
		}
		net += dir * l.EntryPrice
		if l.Strike < lo {
			lo = l.Strike
		}
		if l.Strike > hi {
			hi = l.Strike
		}
	}
	if math.IsInf(lo, 1) || math.IsInf(hi, -1) || hi <= lo {
		return
	}
	wing := hi - lo
	s.NetCredit = math.Round(net*100) / 100
	if isDebitSpreadType(s.Type) {
		s.MaxProfit = math.Round((wing-math.Max(net, 0))*100) / 100
		s.MaxLoss = math.Round(math.Max(net, 0)*100) / 100
	} else {
		s.MaxProfit = math.Round(math.Max(net, 0)*100) / 100
		s.MaxLoss = math.Round(math.Max(wing-net, 0)*100) / 100
	}
	if s.Type == "bull_call" {
		s.LongStrike, s.ShortStrike = lo, hi
	}
	s.WingWidth = math.Round(wing*10000) / 10000
}

// convertToCondor implements ШАГ 2A3: add a far bear wing above a profitable
// call vertical (below a put vertical), targeting breakeven-or-better.
func convertToCondor(s *spreadRecord) ([]string, error) {
	p, found := quant.GetPositionByID(s.PositionID)
	if !found {
		return nil, fmt.Errorf("позиция не найдена")
	}
	isCalls := s.LongStrike < s.ShortStrike
	ref := math.Max(s.ShortStrike, s.LongStrike)
	if !isCalls {
		ref = math.Min(s.ShortStrike, s.LongStrike)
	}
	strikes, find, err := optionChainFor(s.Symbol, s.Expiry)
	if err != nil {
		return nil, err
	}
	sideName := "выше"
	var ks []float64
	if isCalls {
		ks = strikesAbove(strikes, ref, 2)
	} else {
		ks = strikesBelow(strikes, ref, 2)
		sideName = "ниже"
	}
	if len(ks) < 2 {
		return nil, fmt.Errorf("нет двух страйков %s для крыла кондора", sideName)
	}
	o1 := find(ks[0], isCalls)
	if o1 == nil {
		return nil, fmt.Errorf("нет опциона на страйке %.0f", ks[0])
	}
	msg1, err := appendOptionLeg(p, o1, "SELL", s.Qty)
	if err != nil {
		return nil, err
	}
	o2 := find(ks[1], isCalls)
	if o2 == nil {
		return nil, fmt.Errorf("нет опциона на страйке %.0f", ks[1])
	}
	msg2, err := appendOptionLeg(p, o2, "BUY", s.Qty)
	if err != nil {
		return nil, err
	}
	persistReconstruction(s, p, "CONDOR", s.TPR1, s.TPR2)
	return []string{msg1, msg2}, nil
}

// convertToLadder implements ШАГ 2B1: buy q ATM calls and sell 2q of the old
// long call → +q C(K0) -q C(K1) -q C(K2). New decision points are stored.
func convertToLadder(s *spreadRecord) ([]string, error) {
	if s.Type != "bull_call" {
		return nil, fmt.Errorf("лестница определена для bull_call (%s) — выполните вручную", s.Type)
	}
	p, found := quant.GetPositionByID(s.PositionID)
	if !found {
		return nil, fmt.Errorf("позиция не найдена")
	}
	strikes, find, err := optionChainFor(s.Symbol, s.Expiry)
	if err != nil {
		return nil, err
	}
	spot, _ := getSpotPrice(s.Symbol)
	cands := strikesLessThan(strikes, s.LongStrike)
	if len(cands) == 0 {
		return nil, fmt.Errorf("нет страйков ниже K1=%.0f", s.LongStrike)
	}
	K0 := nearestStrikeFromStrikes(cands, spot)
	oBuy := find(K0, true)
	if oBuy == nil {
		return nil, fmt.Errorf("нет колла на страйке %.0f", K0)
	}
	oSell := find(s.LongStrike, true)
	if oSell == nil {
		return nil, fmt.Errorf("нет колла на страйке %.0f", s.LongStrike)
	}
	msg1, err := appendOptionLeg(p, oBuy, "BUY", s.Qty)
	if err != nil {
		return nil, err
	}
	msg2, err := appendOptionLeg(p, oSell, "SELL", 2*s.Qty)
	if err != nil {
		return nil, err
	}
	step := s.LongStrike - K0
	persistReconstruction(s, p, "LADDER", K0-step, s.ShortStrike)
	return []string{msg1, msg2}, nil
}

// convertToRatio implements ШАГ 2B2: sell extra N calls at the short strike.
// With allow_undefined_risk=false the naked tail is capped by a far long wing
// (spec §22).
func convertToRatio(s *spreadRecord) ([]string, error) {
	if s.Type != "bull_call" {
		return nil, fmt.Errorf("ratio определён для bull_call (%s) — выполните вручную", s.Type)
	}
	p, found := quant.GetPositionByID(s.PositionID)
	if !found {
		return nil, fmt.Errorf("позиция не найдена")
	}
	strikes, find, err := optionChainFor(s.Symbol, s.Expiry)
	if err != nil {
		return nil, err
	}
	oShort := find(s.ShortStrike, true)
	if oShort == nil {
		return nil, fmt.Errorf("нет колла на страйке %.0f", s.ShortStrike)
	}
	N := s.Qty
	msg1, err := appendOptionLeg(p, oShort, "SELL", N)
	if err != nil {
		return nil, err
	}
	orders := []string{msg1}
	upperRef := s.ShortStrike
	if !s.AllowUndefined {
		if ks := strikesAbove(strikes, s.ShortStrike, 1); len(ks) == 1 {
			if oWing := find(ks[0], true); oWing != nil {
				if msg2, err := appendOptionLeg(p, oWing, "BUY", N); err == nil {
					orders = append(orders, msg2+" (крыло)")
					upperRef = ks[0]
				}
			}
		}
	}
	D := math.Max(-s.NetCredit, 0)
	buffer := 0.01 * s.EntrySpot
	be := upperRef + math.Max(float64(s.Qty)*(s.ShortStrike-s.LongStrike)-D, 0)/float64(N) - buffer
	persistReconstruction(s, p, "RATIO", s.TPR1, be)
	return orders, nil
}

// addATMPut implements ШАГ 2B3: buy m ATM puts (m = ⌈q/2⌉) adding downside
// convexity while keeping the right wing profitable.
func addATMPut(s *spreadRecord) ([]string, error) {
	p, found := quant.GetPositionByID(s.PositionID)
	if !found {
		return nil, fmt.Errorf("позиция не найдена")
	}
	strikes, find, err := optionChainFor(s.Symbol, s.Expiry)
	if err != nil {
		return nil, err
	}
	spot, _ := getSpotPrice(s.Symbol)
	if spot <= 0 {
		spot = s.EntrySpot
	}
	Kp := nearestStrikeFromStrikes(strikes, spot)
	oPut := find(Kp, false)
	if oPut == nil {
		return nil, fmt.Errorf("нет пута на страйке %.0f", Kp)
	}
	m := int(math.Ceil(float64(s.Qty) / 2))
	msg, err := appendOptionLeg(p, oPut, "BUY", m)
	if err != nil {
		return nil, err
	}
	persistReconstruction(s, p, "BACKSPREAD_LIKE", s.TPR1, s.TPR2)
	return []string{msg}, nil
}

// buyBackFarShort implements ladder ШАГ 3B1: close the highest short call so
// the ladder returns to a plain vertical.
func buyBackFarShort(s *spreadRecord) ([]string, error) {
	p, found := quant.GetPositionByID(s.PositionID)
	if !found {
		return nil, fmt.Errorf("позиция не найдена")
	}
	top := 0.0
	for _, l := range p.Legs {
		if l.Kind == "OPTION" && l.IsCall && l.Side == "SELL" && l.Strike > top {
			top = l.Strike
		}
	}
	if top == 0 {
		return nil, fmt.Errorf("дальний шорт не найден")
	}
	n := reduceLegs(p, true, top, "SELL", math.MaxInt)
	if n == 0 {
		return nil, fmt.Errorf("не удалось закрыть дальний шорт")
	}
	recomputeVerticalEcon(s, p)
	persistReconstruction(s, p, "VERTICAL", 0, 0)
	return []string{fmt.Sprintf("BUY %d CALL %.0f (закрытие дальнего шорта)", n, top)}, nil
}

// buyBackExtraShorts implements ratio ШАГ 3B1: buy back the extra shorts at
// the short strike, returning to the original vertical.
func buyBackExtraShorts(s *spreadRecord) ([]string, error) {
	p, found := quant.GetPositionByID(s.PositionID)
	if !found {
		return nil, fmt.Errorf("позиция не найдена")
	}
	total := 0
	for _, l := range p.Legs {
		if l.Kind == "OPTION" && l.IsCall && l.Side == "SELL" && l.Strike == s.ShortStrike {
			total += l.Quantity
		}
	}
	extra := total - s.Qty
	if extra <= 0 {
		return nil, fmt.Errorf("дополнительные короткие коллы не найдены")
	}
	n := reduceLegs(p, true, s.ShortStrike, "SELL", extra)
	recomputeVerticalEcon(s, p)
	persistReconstruction(s, p, "VERTICAL", 0, 0)
	return []string{fmt.Sprintf("BUY %d CALL %.0f (откуп доп. шортов)", n, s.ShortStrike)}, nil
}

// shiftLadderLeft implements ladder ШАГ 3B3: rebuild the whole ladder one
// strike block lower (+q C(K0−step), −2q C(K0), close far short C(K2)).
func shiftLadderLeft(s *spreadRecord) ([]string, error) {
	p, found := quant.GetPositionByID(s.PositionID)
	if !found {
		return nil, fmt.Errorf("позиция не найдена")
	}
	set := map[float64]bool{}
	var ks []float64
	for _, l := range p.Legs {
		if l.Kind == "OPTION" && l.IsCall && !set[l.Strike] {
			set[l.Strike] = true
			ks = append(ks, l.Strike)
		}
	}
	sort.Float64s(ks)
	if len(ks) < 3 {
		return nil, fmt.Errorf("структура лестницы не распознана")
	}
	K0, K1, K2 := ks[0], ks[1], ks[2]
	step := K1 - K0
	if step <= 0 {
		return nil, fmt.Errorf("некорректный шаг страйков")
	}
	strikes, find, err := optionChainFor(s.Symbol, s.Expiry)
	if err != nil {
		return nil, err
	}
	newK0 := K0 - step
	exists := false
	for _, st := range strikes {
		if st == newK0 {
			exists = true
		}
	}
	if !exists {
		return nil, fmt.Errorf("нет страйка %.0f для сдвига влево", newK0)
	}
	oNew := find(newK0, true)
	if oNew == nil {
		return nil, fmt.Errorf("нет колла на страйке %.0f", newK0)
	}
	msg1, err := appendOptionLeg(p, oNew, "BUY", s.Qty)
	if err != nil {
		return nil, err
	}
	oOld := find(K0, true)
	if oOld == nil {
		return nil, fmt.Errorf("нет колла на страйке %.0f", K0)
	}
	msg2, err := appendOptionLeg(p, oOld, "SELL", 2*s.Qty)
	if err != nil {
		return nil, err
	}
	reduceLegs(p, true, K2, "SELL", math.MaxInt)
	orders := []string{
		msg1,
		msg2,
		fmt.Sprintf("BUY %d CALL %.0f (закрытие дальнего шорта)", s.Qty, K2),
	}
	persistReconstruction(s, p, "LADDER", s.TPR1-step, s.TPR2-step)
	return orders, nil
}

// defaultAutoRollDTE returns the roll-by-time default from KNOWLEDGE.md §1.3:
// long series roll at 21 DTE, weekly MOEX series on the last full week (7
// DTE); contracts already in their final week start with the rule disabled.
func defaultAutoRollDTE(dte int) int {
	switch {
	case dte > 45:
		return 21
	case dte > 8:
		return 7
	default:
		return 0
	}
}

// execSpreadAction executes a decided automatic action. Rolls and hedges reuse
// the same position logic as the manual endpoints but do not write HTTP output.
func execSpreadAction(s *spreadRecord, run *managerRun) {
	switch run.Action {
	case "ROLL":
		roll := nextRollSeries(s.Symbol, s.Expiry)
		if roll.NextSeries == "" {
			run.Action = "NONE"
			run.Detail = "Нет следующей серии для ролла"
			return
		}
		plan, err := autoRollSpread(s, roll)
		if err != nil {
			run.Action = "NONE"
			if errors.Is(err, errRollDebit) {
				run.Detail = "Держим до экспирации: ролл только за дебет (" + run.Detail + ")"
			} else {
				run.Detail = "Ошибка ролла: " + err.Error()
			}
			return
		}
		run.Live = s.Live
		run.Detail = fmt.Sprintf("Авто-ролл → %s (кредит %.2f) (%s)", roll.NextExpiry, plan.NetCredit, run.Detail)
	case "HEDGE":
		pos, found := quant.GetPositionByID(s.PositionID)
		if !found {
			run.Action = "NONE"
			run.Detail = "Позиция не найдена"
			return
		}
		repricePosition(pos)
		hedgeQty := int(math.Round(-pos.Delta))
		if hedgeQty == 0 {
			run.Action = "NONE"
			run.Detail = "Дельта уже ≈ 0"
			return
		}
		if err := autoHedgePosition(pos, hedgeQty); err != nil {
			run.Action = "NONE"
			run.Detail = "Ошибка хеджа: " + err.Error()
			return
		}
		run.Live = s.Live
		run.Detail = fmt.Sprintf("Авто-хедж: %d контрактов (Δ %.2f)", hedgeQty, pos.Delta)
	case "CLOSE":
		pos, found := quant.RemovePosition(s.PositionID)
		if !found {
			run.Action = "NONE"
			run.Detail = "Позиция не найдена"
			return
		}
		repricePosition(&pos)
		stopTrade := quant.Trade{
			ID:          fmt.Sprintf("trd-%d", time.Now().Unix()),
			Strategy:    pos.Strategy,
			Symbol:      pos.Symbol,
			OpenedAt:    pos.OpenedAt,
			ClosedAt:    time.Now(),
			EntryValue:  pos.EntryValue,
			ExitValue:   pos.CurrentValue,
			RealizedPnL: pos.PnL,
			PnLPercent:  pos.PnLPercent,
		}
		enrichTradeContext(&stopTrade, s.Symbol, s.Expiry, s.EntrySpot)
		quant.AddTrade(stopTrade)
		s.Status = "CLOSED"
		saveSpreadRecord(*s)
		reason := run.Detail
		if reason == "" {
			reason = "Закрыто авто-менеджером"
		}
		notifyStructureClosed(s, reason, stopTrade.RealizedPnL, stopTrade.PnLPercent)
		run.Live = s.Live
		run.Detail = "Стоп-лосс исполнен (" + run.Detail + ")"
	case "REVIEW":
		// Decision-point alert (spec §6): no orders until the user sets a view.
		run.Detail = "⚠ " + run.Detail
	case "ROLL_PROFIT":
		if err := rollProfitOnTarget(s); err != nil {
			run.Action = "NONE"
			run.Detail = "Ролл на прибыль не выполнен: " + err.Error()
			return
		}
		run.Live = s.Live
	case "CONVERT_CONDOR":
		orders, err := convertToCondor(s)
		finishReconstruction(s, run, "CONDOR", orders, err)
	case "CONVERT_LADDER":
		orders, err := convertToLadder(s)
		finishReconstruction(s, run, "LADDER", orders, err)
	case "CONVERT_RATIO":
		orders, err := convertToRatio(s)
		finishReconstruction(s, run, "RATIO", orders, err)
	case "ADD_ATM_PUT":
		orders, err := addATMPut(s)
		finishReconstruction(s, run, "BACKSPREAD_LIKE", orders, err)
	case "BUYBACK_FAR_SHORT":
		orders, err := buyBackFarShort(s)
		finishReconstruction(s, run, "VERTICAL", orders, err)
	case "BUYBACK_EXTRA":
		orders, err := buyBackExtraShorts(s)
		finishReconstruction(s, run, "VERTICAL", orders, err)
	case "SHIFT_LEFT":
		orders, err := shiftLadderLeft(s)
		finishReconstruction(s, run, "LADDER", orders, err)
	}
}

// finishReconstruction stores the executor outcome in the manager log entry.
func finishReconstruction(s *spreadRecord, run *managerRun, state string, orders []string, err error) {
	if err != nil {
		run.Action = "NONE"
		run.Detail = run.Detail + " — не выполнено: " + err.Error()
		return
	}
	run.Live = s.Live
	run.Orders = orders
	run.Detail = fmt.Sprintf("%s → состояние %s. %s", run.Detail, state, strings.Join(orders, "; "))
}

// closeSpreadPosition closes the linked position, records the trade and marks
// the spread ROLLED (kept open as history).
func closeSpreadPosition(s *spreadRecord) error {
	pos, found := quant.RemovePosition(s.PositionID)
	if !found {
		return fmt.Errorf("linked position not found")
	}
	repricePosition(&pos)
	ctxTrade := quant.Trade{
		ID:          fmt.Sprintf("trd-%d", time.Now().Unix()),
		Strategy:    pos.Strategy,
		Symbol:      pos.Symbol,
		OpenedAt:    pos.OpenedAt,
		ClosedAt:    time.Now(),
		EntryValue:  pos.EntryValue,
		ExitValue:   pos.CurrentValue,
		RealizedPnL: pos.PnL,
		PnLPercent:  pos.PnLPercent,
	}
	enrichTradeContext(&ctxTrade, s.Symbol, s.Expiry, s.EntrySpot)
	quant.AddTrade(ctxTrade)
	s.Status = "ROLLED"
	saveSpreadRecord(*s)
	return nil
}

// createFromPlan opens a fresh vertical from a plan, copying all management
// rules from the source record. Used by time-rolls and profit rolls alike.
func createFromPlan(plan *spreadPlan, src *spreadRecord, qty, rollCount int) (*spreadRecord, error) {
	mult := contractMultiplier(plan.Symbol)
	p := quant.Position{
		ID:       fmt.Sprintf("pos-%d", time.Now().UnixNano()/1e6),
		Strategy: plan.DisplayName,
		Symbol:   plan.Symbol,
		Expiry:   plan.Expiry,
		OpenedAt: time.Now(),
	}
	for _, l := range plan.Legs {
		p.Legs = append(p.Legs, quant.PositionLeg{
			SecID:        l.SecID,
			Symbol:       plan.Symbol,
			Kind:         "OPTION",
			Side:         l.Side,
			Quantity:     qty,
			Strike:       l.Strike,
			IsCall:       l.IsCall,
			EntryPrice:   l.Price,
			CurrentPrice: l.Price,
		})
		if l.Side == "SELL" {
			p.Margin += l.MarginShort * float64(qty)
		} else {
			p.Margin += plan.MaxLoss * mult * float64(qty)
		}
	}
	repricePosition(&p)
	quant.SavePosition(p)

	rec := spreadRecord{
		ID:                fmt.Sprintf("spr-%d", time.Now().UnixNano()/1e6),
		PositionID:        p.ID,
		Symbol:            plan.Symbol,
		Type:              plan.Type,
		DisplayName:       plan.DisplayName,
		Expiry:            plan.Expiry,
		Qty:               qty,
		ShortStrike:       plan.ShortStrike,
		LongStrike:        plan.LongStrike,
		WingWidth:         plan.WingWidth,
		NetCredit:         plan.NetCredit,
		MaxProfit:         plan.MaxProfit,
		MaxLoss:           plan.MaxLoss,
		Margin:            plan.MarginShort,
		OpenedAt:          time.Now().Format(time.RFC3339),
		Status:            "OPEN",
		RollCount:         rollCount,
		State:             "VERTICAL",
		EntrySpot:         plan.Spot,
		StopLossPct:       src.StopLossPct,
		TakeProfitPct:     src.TakeProfitPct,
		TrailingStopPct:   src.TrailingStopPct,
		MaxHedgeDelta:     src.MaxHedgeDelta,
		AutoRollDTE:       src.AutoRollDTE,
		RollCreditPct:     src.RollCreditPct,
		RollStrikeRiskPct: src.RollStrikeRiskPct,
		AutoHedge:         src.AutoHedge,
		Live:              src.Live,
		ProfitTargetPct:   src.ProfitTargetPct,
		ProfitAction:      src.ProfitAction,
		TPRMode:           src.TPRMode,
		TPRSigmaMult:      src.TPRSigmaMult,
		SigmaAnnual:       src.SigmaAnnual,
		RollAlpha:         src.RollAlpha,
		AllowUndefined:    src.AllowUndefined,
		ViewOverride:      src.ViewOverride,
		CentralStrike:     plan.CentralStrike,
	}
	saveSpreadRecord(rec)
	return &rec, nil
}

// autoRollSpread performs the same sequence as spreadRollHandler (close +
// reopen in the next series) but without HTTP plumbing. The new plan is built
// first so a failure leaves the existing position untouched. Credit spreads are
// rolled only when the new series opens for a net credit; a debit roll returns
// errRollDebit and the caller keeps the current position.
func autoRollSpread(s *spreadRecord, roll nextSeries) (*spreadPlan, error) {
	plan, err := buildVerticalSpread(s.Symbol, s.Type, roll.NextExpiry, s.Qty)
	if err != nil {
		return nil, err
	}
	if !isDebitSpreadType(s.Type) && plan.NetCredit <= 0 {
		return nil, errRollDebit
	}
	if err := closeSpreadPosition(s); err != nil {
		return nil, err
	}
	if _, err := createFromPlan(plan, s, s.Qty, s.RollCount+1); err != nil {
		return nil, err
	}
	return plan, nil
}

// rollProfitOnTarget implements ШАГ 2A2 of the management spec: close the
// winning spread and reopen a fresh one risking at most α × realized profit.
func rollProfitOnTarget(s *spreadRecord) error {
	pos, found := quant.GetPositionByID(s.PositionID)
	if !found {
		return fmt.Errorf("позиция не найдена")
	}
	repricePosition(pos)
	realized := pos.PnL

	plan, err := buildVerticalSpread(s.Symbol, s.Type, s.Expiry, 1)
	if err != nil {
		return err
	}
	riskPerCtr := plan.MaxLoss * contractMultiplier(plan.Symbol)
	if riskPerCtr <= 0 {
		return fmt.Errorf("некорректный риск на контракт")
	}

	alpha := s.RollAlpha
	if alpha <= 0 {
		alpha = 1
	}
	qtyNew := int(math.Floor(alpha * realized / riskPerCtr))

	if err := closeSpreadPosition(s); err != nil {
		return err
	}
	if qtyNew >= 1 {
		_, err = createFromPlan(plan, s, qtyNew, s.RollCount)
		return err
	}
	// Not enough profit to fund a new structure — the T/P close stands.
	s.Status = "CLOSED"
	saveSpreadRecord(*s)
	return nil
}

// autoHedgePosition places a delta hedge (paper unless the spread is live) and
// records the FUTURES/SHARES leg on the position.
func autoHedgePosition(pos *quant.Position, hedgeQty int) error {
	side := "BUY"
	if hedgeQty < 0 {
		side = "SELL"
		hedgeQty = -hedgeQty
	}
	futureSecid := selectedSeries[pos.Symbol]
	if futureSecid == "" {
		return fmt.Errorf("no hedge series for %s", pos.Symbol)
	}
	if _, isEquity := equityOptions[pos.Symbol]; isEquity {
		futureSecid = pos.Symbol
	}

	spot, _ := getSpotPrice(pos.Symbol)
	if spot <= 0 {
		spot = pos.CurrentValue
	}
	mult := contractMultiplier(pos.Symbol)
	margin := moexFutureInitialMargin(futureSecid)
	if margin <= 0 {
		margin = spot * mult * 0.15
	}

	pos.Legs = append(pos.Legs, quant.PositionLeg{
		SecID:        futureSecid,
		Symbol:       pos.Symbol,
		Kind:         "FUTURES",
		Side:         side,
		Quantity:     hedgeQty,
		EntryPrice:   spot,
		CurrentPrice: spot,
	})
	pos.Margin += margin * float64(hedgeQty)
	repricePosition(pos)
	quant.SavePosition(*pos)
	return nil
}

func isDebitSpreadType(t string) bool {
	meta, ok := spreadTypes[t]
	return ok && meta.Debit
}

// spreadRulesHandler updates the management rules of an open spread.
// POST /api/v1/spreads/rules {"id":"spr-...","rules":{...}}
func spreadRulesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID    string      `json:"id"`
		Rules spreadRules `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	s, found := spreadRecordByID(req.ID)
	if !found || s.Status != "OPEN" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "spread not found or not open"})
		return
	}
	s.AutoRollDTE = req.Rules.AutoRollDTE
	s.RollCreditPct = req.Rules.RollCreditPct
	s.RollStrikeRiskPct = req.Rules.RollStrikeRiskPct
	s.StopLossPct = req.Rules.StopLossPct
	s.AutoHedge = req.Rules.AutoHedge
	s.MaxHedgeDelta = req.Rules.MaxHedgeDelta
	s.Live = req.Rules.Live
	s.ProfitTargetPct = req.Rules.ProfitTargetPct
	s.ProfitAction = strings.ToUpper(req.Rules.ProfitAction)
	s.TPRMode = strings.ToUpper(req.Rules.TPRMode)
	s.TPRSigmaMult = req.Rules.TPRSigmaMult
	s.SigmaAnnual = req.Rules.SigmaAnnual
	s.RollAlpha = req.Rules.RollAlpha
	s.AllowUndefined = req.Rules.AllowUndefined
	s.ViewOverride = strings.ToUpper(req.Rules.ViewOverride)
	saveSpreadRecord(s)

	if !spreadManagerEnabled() {
		startSpreadManager()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"spread":  s,
		"note":    "Правила применены. Авто-менеджер активен (проверка каждые 60 сек).",
	})
}

// spreadManagerHandler returns the manager status and the last evaluation log.
// URL: /api/v1/spreads/manager
func spreadManagerHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	spreadManagerMu.Lock()
	defer spreadManagerMu.Unlock()
	logCopy := make([]managerRun, len(spreadManagerLog))
	copy(logCopy, spreadManagerLog)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled":    spreadManagerOn,
		"started_at": spreadManagerStart.Format(time.RFC3339),
		"runs":       spreadManagerRuns,
		"log":        logCopy,
	})
}
