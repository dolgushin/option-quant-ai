package main

import (
	"math/rand"
	"testing"
)

func TestSimulateSpreadPnLDeterministic(t *testing.T) {
	mk := func() []float64 {
		return simulateSpreadPnL(80, 1420, 86200, 0.20, 86000, 85500, 20, 500,
			rand.New(rand.NewSource(42)))
	}
	a, b := mk(), mk()
	if len(a) != 500 || len(b) != 500 {
		t.Fatalf("lengths %d/%d, want 500/500", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed diverged at %d: %v vs %v", i, a[i], b[i])
		}
		if i > 0 && a[i] < a[i-1] {
			t.Fatalf("output not sorted at %d", i)
		}
	}
}

func TestSimulateSpreadPnLDeepOTM(t *testing.T) {
	// Spot far above a bull-put spread: every path keeps the credit.
	pnls := simulateSpreadPnL(80, 1420, 120000, 0.20, 86000, 85500, 20, 500,
		rand.New(rand.NewSource(7)))
	probs, sum := summarizePnL(pnls)
	if probs != 500 {
		t.Fatalf("deep OTM must always profit, got %v/500", probs)
	}
	if avg := sum / 500; avg != 80 {
		t.Fatalf("deep OTM avg = %v, want credit 80", avg)
	}
}

func TestScanSpreadGridRanksByExpectancy(t *testing.T) {
	rows := scanSpreadGrid(80, 86200,
		[]float64{86000, 90000}, []float64{500},
		[]int{14}, []float64{0.20}, 500, 2)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want top 2", len(rows))
	}
	// Far-OTM 90000-short collects the credit almost untouched, so it tops
	// expectancy; near-spot 86000 pays for its frequent small losses.
	if rows[0].ShortK != 90000 {
		t.Fatalf("top row short = %v, want far-OTM 90000", rows[0].ShortK)
	}
	if rows[0].AvgPnL < rows[1].AvgPnL {
		t.Fatal("rows not sorted by expectancy desc")
	}
	// Both wing sides are scanned: with a wide grid both orientations appear.
	full := scanSpreadGrid(80, 86200,
		[]float64{86000}, []float64{500},
		[]int{14}, []float64{0.20}, 200, 0)
	if len(full) != 2 {
		t.Fatalf("one short × one wing must give 2 orientations, got %d", len(full))
	}
	if !((full[0].LongK == 85500 && full[1].LongK == 86500) ||
		(full[0].LongK == 86500 && full[1].LongK == 85500)) {
		t.Fatalf("orientations wrong: %+v", full)
	}
}
