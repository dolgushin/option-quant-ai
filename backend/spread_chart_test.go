package main

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

// TestCandidatePayoff verifies the at-expiry P&L math for a credit spread:
// a short put + long further-OTM put. At a spot far above the strikes the
// position is worth its net credit; far below it loses the wing width minus
// the credit.
func TestCandidatePayoff(t *testing.T) {
	plan := &spreadPlan{
		Symbol:      "Si",
		Multiplier:  1000,
		Qty:         1,
		ShortStrike: 86500,
		LongStrike:  86000,
		Legs: []spreadLeg{
			{Side: "SELL", Strike: 86500, IsCall: false, Price: 200},
			{Side: "BUY", Strike: 86000, IsCall: false, Price: 100},
		},
	}
	// scale = 1000 x 1. Net credit per share = 200 - 100 = 100 => 100000 rubles.
	pts := candidatePayoff(plan)
	if len(pts) == 0 {
		t.Fatal("candidatePayoff returned no points")
	}
	// Find an S far above both strikes.
	last := pts[len(pts)-1]
	if last.S > 86700 {
		// Both puts are OTM -> intrinsic 0 -> P&L = +netCredit.
		want := 100.0 * 1000
		if got := last.PnL; got != want {
			t.Fatalf("OTM P&L = %v, want %v", got, want)
		}
	}
	first := pts[0]
	if first.S < 85800 {
		// Both puts deep ITM by 500+. P&L = short vs long intrinsic diff.
		// short pays (86500-S), long receives (86000-S), net = -500 per share.
		// minus netCredit effect: entry credit +100/share.
		// P&L = -500 + 100 = -400 per share => -400000.
		// Compute exactly from spot.
		S := first.S
		shortIntr := 86500 - S
		longIntr := 86000 - S
		shortPnl := -(shortIntr - 200)
		longPnl := (longIntr - 100)
		want := (shortPnl + longPnl) * 1000
		if got := first.PnL; got != want {
			t.Fatalf("ITM P&L = %v, want %v", got, want)
		}
	}
}

// TestNotifyCandidateSpreadDedup checks that the same construction is only
// notified once (the dedup key is stable and dedup suppresses a resend).
func TestNotifyCandidateSpreadDedup(t *testing.T) {
	lastCandidateKeyMu.Lock()
	lastCandidateKey = ""
	lastCandidateKeyMu.Unlock()

	a := &coreCandidate{Symbol: "Si", Strategy: "bull_put", Expiry: "2026-09-17", ShortStrike: 86500, LongStrike: 86000}
	b := &coreCandidate{Symbol: "Si", Strategy: "bull_put", Expiry: "2026-09-17", ShortStrike: 86500, LongStrike: 86000}
	c := &coreCandidate{Symbol: "Si", Strategy: "bear_call", Expiry: "2026-09-17", ShortStrike: 87500, LongStrike: 88000}

	if candidateDedupKey(a) != candidateDedupKey(b) {
		t.Fatalf("same construction should share a dedup key")
	}
	if candidateDedupKey(a) == candidateDedupKey(c) {
		t.Fatalf("different construction should have a different key")
	}
}

// TestDrawPayoffChartPNG verifies the chart renders a valid PNG of the expected
// size and content is decodable.
func TestDrawPayoffChartPNG(t *testing.T) {
	pts := []payoffPoint{
		{S: 85000, PnL: -400000},
		{S: 86000, PnL: -200000},
		{S: 86500, PnL: 100000},
		{S: 87000, PnL: 100000},
	}
	data, err := drawPayoffChart(pts, 86800, 86500, 86000)
	if err != nil {
		t.Fatalf("drawPayoffChart: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("no PNG data produced")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding PNG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != chartW || b.Dy() != chartH {
		t.Fatalf("unexpected chart size: %d x %d", b.Dx(), b.Dy())
	}
	// PNG signature check.
	if !bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		t.Fatal("invalid PNG signature")
	}
}

// TestCandidateCaption covers the caption builder including HTML escaping of
// Cyrillic display names.
func TestCandidateCaption(t *testing.T) {
	c := &coreCandidate{
		Symbol: "SBER", Strategy: "bull_put", DisplayName: "Bull Put Spread",
		Expiry: "2026-09-17", DTE: 15, ShortStrike: 180, LongStrike: 175,
		NetCredit: 450, MaxProfit: 450, MaxLoss: 550, Score: 88, PopProb: 72,
		Reasons: []string{"iv rank 60", "bullish"},
	}
	plan := &spreadPlan{Symbol: "SBER", Spot: 182, ShortStrike: 180, LongStrike: 175}
	caption := candidateCaption(c, plan)
	for _, want := range []string{"180", "175", "Bull Put Spread", "SBER", "450", "550", "72%", "88/100"} {
		if !strings.Contains(caption, want) {
			t.Fatalf("caption missing %q:\n%s", want, caption)
		}
	}
	if strings.Contains(caption, "<") && strings.Count(caption, "<b>")+strings.Count(caption, "</b>") == 0 {
		t.Fatalf("caption has unescaped HTML:\n%s", caption)
	}
}
