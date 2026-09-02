package main

import (
	"math"
	"path/filepath"
	"testing"

	"option-quant-ai/quant"
)

func withRules(base spreadRecord, f func(*spreadRecord)) spreadRecord {
	r := base
	f(&r)
	return r
}

func TestDecideSpreadActionPriority(t *testing.T) {
	base := spreadRecord{
		ID:          "spr-t",
		Symbol:      "SBER", // multiplier 100
		Type:        "bull_put",
		Qty:         1,
		MaxProfit:   1.0, // ₽/share → 100 ₽ position-level
		MaxLoss:     9.0, // ₽/share → 900 ₽ position-level
		ShortStrike: 260,
	}

	tests := []struct {
		name   string
		rec    spreadRecord
		dte    int
		delta  float64
		pnl    float64
		spot   float64
		spotOK bool
		want   string
	}{
		{"no rules enabled", base, 30, 0, 0, 0, false, "NONE"},
		{"stop-loss fires before roll rules", withRules(base, func(r *spreadRecord) { r.StopLossPct = 0.75; r.AutoRollDTE = 20 }), 10, 0, -700, 0, false, "CLOSE"},
		{"stop-loss below threshold", withRules(base, func(r *spreadRecord) { r.StopLossPct = 0.75 }), 30, 0, -600, 0, false, "NONE"},
		{"dte trigger at boundary", withRules(base, func(r *spreadRecord) { r.AutoRollDTE = 21 }), 21, 0, 0, 0, false, "ROLL"},
		{"captured credit trigger", withRules(base, func(r *spreadRecord) { r.RollCreditPct = 0.5 }), 30, 0, 60, 0, false, "ROLL"},
		{"captured credit ignores debit spreads", withRules(base, func(r *spreadRecord) { r.RollCreditPct = 0.5; r.Type = "bull_call" }), 30, 0, 60, 0, false, "NONE"},
		{"strike proximity trigger", withRules(base, func(r *spreadRecord) { r.RollStrikeRiskPct = 0.02 }), 30, 0, 0, 258, true, "ROLL"},
		{"strike proximity needs spot", withRules(base, func(r *spreadRecord) { r.RollStrikeRiskPct = 0.02 }), 30, 0, 0, 258, false, "NONE"},
		{"hedge trigger", withRules(base, func(r *spreadRecord) { r.AutoHedge = true; r.MaxHedgeDelta = 1 }), 30, 2.5, 0, 0, false, "HEDGE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideSpreadAction(tt.rec, tt.dte, tt.delta, tt.pnl, tt.spot, 0.30, tt.spotOK)
			if got.Action != tt.want {
				t.Fatalf("action = %q (%s), want %q", got.Action, got.Detail, tt.want)
			}
		})
	}
}

func TestDecideSpreadActionCapturedPctUnits(t *testing.T) {
	rec := spreadRecord{Symbol: "SBER", Type: "bull_put", Qty: 2, MaxProfit: 1.0} // 2 × 100 = 200 ₽ credit
	run := decideSpreadAction(rec, 30, 0, 100, 0, 0.30, false)
	if math.Abs(run.CapturedPct-0.5) > 1e-9 {
		t.Fatalf("captured pct = %v, want 0.5 (multiplier/qty scaling broken)", run.CapturedPct)
	}
	if math.Abs(run.MaxProfit-200) > 1e-9 {
		t.Fatalf("max profit = %v, want 200 ₽", run.MaxProfit)
	}
}

func TestRunSpreadManagerPassSkipsPaper(t *testing.T) {
	dataDir := t.TempDir()
	initSpreads(dataDir)

	paper := spreadRecord{
		ID:          "spr-paper",
		PositionID:  "pos-paper",
		Symbol:      "SBER",
		Type:        "bull_put",
		Expiry:      "2026-09-16",
		Qty:         1,
		Status:      "OPEN",
		Live:        false, // Core autoscan paper position
		AutoRollDTE: 21,    // would fire ROLL at DTE ≤ 21
		MaxProfit:   1,
		MaxLoss:     9,
	}
	saveSpreadRecord(paper)

	runSpreadManagerPass()

	if _, found := spreadRecordByID("spr-paper"); !found {
		t.Fatalf("paper spread should remain registered")
	}
	if s, _ := spreadRecordByID("spr-paper"); s.Status != "OPEN" {
		t.Fatalf("paper spread status = %q, want OPEN (must not be managed)", s.Status)
	}
	if s, _ := spreadRecordByID("spr-paper"); s.RollCount != 0 {
		t.Fatalf("paper spread rolled without permission: RollCount = %d", s.RollCount)
	}
	spreadManagerMu.Lock()
	logLen := len(spreadManagerLog)
	spreadManagerMu.Unlock()
	if logLen != 0 {
		t.Fatalf("manager log has %d entries, want 0 (paper spread must be skipped)", logLen)
	}
}

func TestAccumulateBookClose(t *testing.T) {
	// Bull put 86500/86000, qty 4, multiplier 100 → credit structure.
	// Close: SELL (short put) bought back at ask 3003, BUY (long put) sold at
	// bid 1297 → exorbitant debit, vastly worse than theo.
	cands := []bookLegBook{
		{leg: quant.PositionLeg{SecID: "Si86500BX6", Side: "SELL", Kind: "OPTION", Quantity: 4}, price: 3003.0, depth: 1},
		{leg: quant.PositionLeg{SecID: "Si86000BX6", Side: "BUY", Kind: "OPTION", Quantity: 4}, price: 1297.0, depth: 1},
	}
	const mult = 100.0
	total, perLeg, ok := accumulateBookClose(cands, mult)
	if !ok {
		t.Fatal("expected ok=true with two candidates")
	}
	// total = (−3003 + 1297) × 100 × 4 = −1706 × 400 = −682 400 ₽
	want := (-3003.0 + 1297.0) * mult * 4
	if math.Abs(total-want) > 1e-6 {
		t.Fatalf("total = %.2f, want %.2f", total, want)
	}
	if len(perLeg) != 2 {
		t.Fatalf("perLeg len = %d, want 2", len(perLeg))
	}
	// Depth: only 1 lot per book level < 4 qty → breaker flags thin book.
	if perLeg[0].Depth != 1 {
		t.Fatalf("short leg depth = %d, want 1", perLeg[0].Depth)
	}
	if perLeg[0].Price != 3003.0 {
		t.Fatalf("short leg close price = %.2f, want 3003 (bought back at ask)", perLeg[0].Price)
	}
	if perLeg[1].Price != 1297.0 {
		t.Fatalf("long leg close price = %.2f, want 1297 (sold at bid)", perLeg[1].Price)
	}

	// Empty candidates → not ok.
	_, _, ok2 := accumulateBookClose(nil, mult)
	if ok2 {
		t.Fatal("expected ok=false with no candidates")
	}
}

func TestDefaultAutoRollDTE(t *testing.T) {
	cases := map[int]int{60: 21, 46: 21, 45: 7, 14: 7, 9: 7, 8: 0, 3: 0}
	for dte, want := range cases {
		if got := defaultAutoRollDTE(dte); got != want {
			t.Fatalf("defaultAutoRollDTE(%d) = %d, want %d", dte, got, want)
		}
	}
}

func TestExecSpreadActionClosesPosition(t *testing.T) {
	dataDir := t.TempDir()
	quant.SetDataFile(filepath.Join(dataDir, "portfolio.json"))
	quant.Load()
	initSpreads(dataDir)

	pos := quant.Position{
		ID:       "pos-test-close",
		Strategy: "Bull Put Spread",
		Symbol:   "SBER",
		Expiry:   "2026-09-16",
		Legs: []quant.PositionLeg{
			{SecID: "SR260CU6", Symbol: "SBER", Kind: "OPTION", Side: "SELL", Quantity: 1, Strike: 260, EntryPrice: 1.8, CurrentPrice: 1.8},
			{SecID: "SP250CU6", Symbol: "SBER", Kind: "OPTION", Side: "BUY", Quantity: 1, Strike: 250, EntryPrice: 0.9, CurrentPrice: 0.9},
		},
	}
	quant.SavePosition(pos)

	rec := spreadRecord{
		ID:          "spr-test-close",
		PositionID:  pos.ID,
		Symbol:      "SBER",
		Type:        "bull_put",
		Expiry:      pos.Expiry,
		Qty:         1,
		Status:      "OPEN",
		StopLossPct: 0.75,
		MaxProfit:   1,
		MaxLoss:     9,
	}
	saveSpreadRecord(rec)

	run := managerRun{Action: "CLOSE", Detail: "тест стопа"}
	execSpreadAction(&rec, &run)

	if run.Action == "NONE" {
		t.Fatalf("CLOSE action failed: %s", run.Detail)
	}
	saved, ok := spreadRecordByID(rec.ID)
	if !ok || saved.Status != "CLOSED" {
		t.Fatalf("spread status = %q (found=%v), want CLOSED", saved.Status, ok)
	}
	if _, found := quant.GetPositionByID(pos.ID); found {
		t.Fatalf("linked position %s should be removed", pos.ID)
	}
	trades := quant.GetTrades()
	if len(trades) == 0 {
		t.Fatalf("expected a recorded trade after stop-out")
	}
}

func TestDecideSpreadStateMachine(t *testing.T) {
	base := spreadRecord{
		ID: "spr-sm", Symbol: "SBER", Type: "bull_call", Qty: 1,
		MaxProfit: 1.0, MaxLoss: 5.0,
		LongStrike: 250, ShortStrike: 260,
		State: "VERTICAL", EntrySpot: 270,
		ProfitTargetPct: 0.75, ProfitAction: "CLOSE",
		TPRMode: "ONE_DAY_SIGMA", TPRSigmaMult: 1, SigmaAnnual: 0.30,
	}

	tests := []struct {
		name string
		mut  func(*spreadRecord)
		dte  int
		pnl  float64
		spot float64
		want string
	}{
		{"profit target closes by default", func(r *spreadRecord) {}, 20, 80, 272, "CLOSE"},
		{"profit target with condor action", func(r *spreadRecord) { r.ProfitAction = "CONDOR" }, 20, 80, 272, "CONVERT_CONDOR"},
		{"profit target with roll action", func(r *spreadRecord) { r.ProfitAction = "ROLL" }, 20, 80, 272, "ROLL_PROFIT"},
		{"tpr sigma without view is review only", func(r *spreadRecord) {}, 20, 0, 264.5, "REVIEW"},
		{"tpr with bullish view builds ladder", func(r *spreadRecord) { r.ViewOverride = "BULLISH" }, 20, 0, 264.5, "CONVERT_LADDER"},
		{"tpr with sideways view builds ratio", func(r *spreadRecord) { r.ViewOverride = "SIDEWAYS" }, 20, 0, 264.5, "CONVERT_RATIO"},
		{"tpr with bearish view adds put", func(r *spreadRecord) { r.ViewOverride = "BEARISH" }, 20, 0, 264.5, "ADD_ATM_PUT"},
		{"ladder tpr2 buys back far short", func(r *spreadRecord) { r.State = "LADDER"; r.TPR2 = 265 }, 20, 0, 266, "BUYBACK_FAR_SHORT"},
		{"ladder tpr1 shifts left", func(r *spreadRecord) { r.State = "LADDER"; r.TPR1 = 255; r.TPR2 = 280 }, 20, 0, 254, "SHIFT_LEFT"},
		{"ratio tpr2 buys back extras", func(r *spreadRecord) { r.State = "RATIO"; r.TPR2 = 268; r.ViewOverride = "" }, 20, 0, 268.5, "BUYBACK_EXTRA"},
		{"reconstructed state time-stops", func(r *spreadRecord) { r.State = "LADDER"; r.AutoRollDTE = 7; r.TPR2 = 300 }, 7, 0, 270, "CLOSE"},
		{"inside sigma band is quiet", func(r *spreadRecord) {}, 20, 0, 269, "NONE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := base
			if tt.mut != nil {
				tt.mut(&rec)
			}
			got := decideSpreadAction(rec, tt.dte, 0, tt.pnl, tt.spot, 0.30, true)
			if got.Action != tt.want {
				t.Fatalf("action = %q (%s), want %q", got.Action, got.Detail, tt.want)
			}
		})
	}
}
