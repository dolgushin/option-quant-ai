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
			got := decideSpreadAction(tt.rec, tt.dte, tt.delta, tt.pnl, tt.spot, tt.spotOK)
			if got.Action != tt.want {
				t.Fatalf("action = %q (%s), want %q", got.Action, got.Detail, tt.want)
			}
		})
	}
}

func TestDecideSpreadActionCapturedPctUnits(t *testing.T) {
	rec := spreadRecord{Symbol: "SBER", Type: "bull_put", Qty: 2, MaxProfit: 1.0} // 2 × 100 = 200 ₽ credit
	run := decideSpreadAction(rec, 30, 0, 100, 0, false)
	if math.Abs(run.CapturedPct-0.5) > 1e-9 {
		t.Fatalf("captured pct = %v, want 0.5 (multiplier/qty scaling broken)", run.CapturedPct)
	}
	if math.Abs(run.MaxProfit-200) > 1e-9 {
		t.Fatalf("max profit = %v, want 200 ₽", run.MaxProfit)
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
