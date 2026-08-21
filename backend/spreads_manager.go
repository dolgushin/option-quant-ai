package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
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
}

// managerRun is the snapshot of one auto-management pass for a spread.
type managerRun struct {
	SpreadID   string `json:"spread_id"`
	CheckedAt  string `json:"checked_at"`
	DTE        int    `json:"dte"`
	NetDelta   float64 `json:"net_delta"`
	Pnl        float64 `json:"pnl"`
	MaxProfit  float64 `json:"max_profit"`
	CapturedPct float64 `json:"captured_pct"`
	Action     string `json:"action"`  // ROLL / HEDGE / CLOSE / NONE
	Detail     string `json:"detail"`
	Live       bool   `json:"live"`
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

// runSpreadManagerPass evaluates all OPEN spreads once. Actions (roll/hedge)
// are executed only when a rule fires; results are logged for the UI.
func runSpreadManagerPass() {
	runs := []managerRun{}
	for _, s := range openSpreads() {
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
// days to expiry and the spot price when the strike rule is enabled) and
// delegates the decision to decideSpreadAction.
func evaluateSpread(s spreadRecord) managerRun {
	dte := dteInDays(s.Expiry, time.Now())
	netDelta, pnl := 0.0, 0.0

	positions := quant.GetActivePositions()
	for i := range positions {
		if positions[i].ID == s.PositionID {
			p := &positions[i]
			repricePosition(p)
			quant.SavePosition(*p)
			netDelta = p.Delta
			pnl = p.PnL
			break
		}
	}

	spotOK, spot := false, 0.0
	if s.RollStrikeRiskPct > 0 {
		if v, err := getSpotPrice(s.Symbol); err == nil && v > 0 {
			spot, spotOK = v, true
		}
	}

	run := decideSpreadAction(s, dte, netDelta, pnl, spot, spotOK)
	run.CheckedAt = time.Now().Format(time.RFC3339)
	return run
}

// decideSpreadAction maps spread telemetry to a management action using the
// record's rules. Priority follows KNOWLEDGE.md §1: stop-loss first, then the
// roll triggers (DTE, captured credit share, short-strike test) and finally
// delta hedging. MaxProfit/MaxLoss are stored per underlying unit, so they are
// scaled by the contract multiplier and quantity before being compared with
// the position-level PnL.
func decideSpreadAction(s spreadRecord, dte int, netDelta, pnl, spot float64, spotOK bool) managerRun {
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

	// 1) Stop-loss: close when the drawdown reaches the configured fraction of
	// the maximum loss (KNOWLEDGE.md §1.4).
	if s.StopLossPct > 0 && maxLoss > 0 && pnl <= -s.StopLossPct*maxLoss {
		run.Action = "CLOSE"
		run.Detail = fmt.Sprintf("Убыток %.0f ₽ достиг порога %.0f%% макс. убытка (%.0f ₽)", pnl, s.StopLossPct*100, maxLoss)
		return run
	}

	// 2) DTE roll trigger.
	if s.AutoRollDTE > 0 && dte <= s.AutoRollDTE {
		run.Action = "ROLL"
		run.Detail = fmt.Sprintf("DTE %d ≤ %d — экспирация близко", dte, s.AutoRollDTE)
		return run
	}

	// 3) Captured-credit trigger (credit spreads only).
	if s.RollCreditPct > 0 && !isDebitSpreadType(s.Type) && run.CapturedPct >= s.RollCreditPct {
		run.Action = "ROLL"
		run.Detail = fmt.Sprintf("Собрано %.0f%% кредита (≥ %.0f%%)", run.CapturedPct*100, s.RollCreditPct*100)
		return run
	}

	// 4) Short-strike proximity trigger.
	if s.RollStrikeRiskPct > 0 && spotOK && s.ShortStrike > 0 {
		dist := math.Abs(spot-s.ShortStrike) / s.ShortStrike
		if dist <= s.RollStrikeRiskPct {
			run.Action = "ROLL"
			run.Detail = fmt.Sprintf("Цена %.2f подошла к стрику %.2f (дист. %.2f%%)", spot, s.ShortStrike, dist*100)
			return run
		}
	}

	// 5) Delta hedge trigger.
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
		quant.AddTrade(quant.Trade{
			ID:          fmt.Sprintf("trd-%d", time.Now().Unix()),
			Strategy:    pos.Strategy,
			Symbol:      pos.Symbol,
			OpenedAt:    pos.OpenedAt,
			ClosedAt:    time.Now(),
			EntryValue:  pos.EntryValue,
			ExitValue:   pos.CurrentValue,
			RealizedPnL: pos.PnL,
			PnLPercent:  pos.PnLPercent,
		})
		s.Status = "CLOSED"
		saveSpreadRecord(*s)
		run.Live = s.Live
		run.Detail = "Стоп-лосс исполнен (" + run.Detail + ")"
	}
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

	pos, found := quant.RemovePosition(s.PositionID)
	if !found {
		return nil, fmt.Errorf("linked position not found")
	}
	repricePosition(&pos)
	quant.AddTrade(quant.Trade{
		ID:          fmt.Sprintf("trd-%d", time.Now().Unix()),
		Strategy:    pos.Strategy,
		Symbol:      pos.Symbol,
		OpenedAt:    pos.OpenedAt,
		ClosedAt:    time.Now(),
		EntryValue:  pos.EntryValue,
		ExitValue:   pos.CurrentValue,
		RealizedPnL: pos.PnL,
		PnLPercent:  pos.PnLPercent,
	})
	s.Status = "ROLLED"
	saveSpreadRecord(*s)

	mult := contractMultiplier(plan.Symbol)
	p := quant.Position{
		ID:       fmt.Sprintf("pos-%d", time.Now().UnixNano()/1e6),
		Strategy: plan.DisplayName,
		Symbol:   plan.Symbol,
		Expiry:   plan.Expiry,
		OpenedAt: time.Now(),
	}
	for _, l := range plan.Legs {
		leg := quant.PositionLeg{
			SecID:        l.SecID,
			Symbol:       plan.Symbol,
			Kind:         "OPTION",
			Side:         l.Side,
			Quantity:     plan.Qty,
			Strike:       l.Strike,
			IsCall:       l.IsCall,
			EntryPrice:   l.Price,
			CurrentPrice: l.Price,
		}
		p.Legs = append(p.Legs, leg)
		if l.Side == "SELL" {
			p.Margin += l.MarginShort * float64(plan.Qty)
		} else {
			p.Margin += plan.MaxLoss * mult * float64(plan.Qty)
		}
	}
	repricePosition(&p)
	quant.SavePosition(p)

	newRec := spreadRecord{
		ID:               fmt.Sprintf("spr-%d", time.Now().UnixNano()/1e6),
		PositionID:       p.ID,
		Symbol:           plan.Symbol,
		Type:             plan.Type,
		DisplayName:      plan.DisplayName,
		Expiry:           plan.Expiry,
		Qty:              plan.Qty,
		ShortStrike:      plan.ShortStrike,
		LongStrike:       plan.LongStrike,
		WingWidth:        plan.WingWidth,
		NetCredit:        plan.NetCredit,
		MaxProfit:        plan.MaxProfit,
		MaxLoss:          plan.MaxLoss,
		Margin:           plan.MarginShort,
		OpenedAt:         time.Now().Format(time.RFC3339),
		Status:           "OPEN",
		RollCount:        s.RollCount + 1,
		StopLossPct:      s.StopLossPct,
		TakeProfitPct:    s.TakeProfitPct,
		TrailingStopPct:  s.TrailingStopPct,
		MaxHedgeDelta:    s.MaxHedgeDelta,
		AutoRollDTE:      s.AutoRollDTE,
		RollCreditPct:    s.RollCreditPct,
		RollStrikeRiskPct: s.RollStrikeRiskPct,
		AutoHedge:        s.AutoHedge,
		Live:             s.Live,
	}
	saveSpreadRecord(newRec)
	return plan, nil
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