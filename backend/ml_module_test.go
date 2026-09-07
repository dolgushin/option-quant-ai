package main

import (
	"testing"
)

func TestEncodeStrategyName(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"Bull Put Spread", 2, true}, // journal display name
		{"bull_put_spread", 2, true}, // API key
		{"bull_put", 2, true},        // short key
		{"Bear Call Spread", 3, true},
		{"bear_call_spread", 3, true},
		{"Bull Call Spread", 4, true},
		{"Bear Put Spread", 5, true},
		{"Iron Condor", 0, true},
		{"iron_condor", 0, true},
		{"IC", 0, true},
		{"iron_butterfly", 1, true},
		{"Long Strangle", 6, true},
		{"vertical", 8, true},
		{"BP", -1, false}, // ambiguous abbreviation stays unknown
		{"something-else", -1, false},
		{"", -1, false},
	}
	for _, tc := range cases {
		got, ok := encodeStrategyName(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("encodeStrategyName(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// testScanModel is a fake model that likes high DTE and nothing else.
func testScanModel() *logisticModel {
	return &logisticModel{
		Weights: []float64{0, 2, 0, 0, 0, 0, 0},
		MinMM: featureMinMax{
			Min: []float64{0, 0, -1, -1, 0, 0},
			Max: []float64{45, 1, 1, 1, 8, 5},
		},
	}
}

func TestScanMLCombinationsTopN(t *testing.T) {
	initSymbolEncodings()
	model := testScanModel()
	rows := scanMLCombinations(model,
		[]string{"Si", "RI"},
		[]string{"bull_put_spread", "bear_call_spread"},
		[]int{7, 14, 30}, []float64{20, 40},
		[]string{"BULLISH", "SIDEWAYS"}, []string{"neutral"},
		3)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want top 3", len(rows))
	}
	// Sorted by win probability descending; DTE-only model → max DTE first.
	for i := 0; i < len(rows)-1; i++ {
		if rows[i].WinProb < rows[i+1].WinProb {
			t.Fatalf("rows not sorted desc: %v then %v", rows[i].WinProb, rows[i+1].WinProb)
		}
	}
	if rows[0].DTE != 30 {
		t.Fatalf("top row DTE = %d, want 30 (model likes DTE)", rows[0].DTE)
	}
	// Unknown strategies are skipped, not scored as iron_condor.
	rows2 := scanMLCombinations(model,
		[]string{"Si"}, []string{"no-such-strategy"},
		[]int{14}, []float64{20}, []string{"BULLISH"}, []string{"neutral"}, 5)
	if len(rows2) != 0 {
		t.Fatalf("unknown strategy produced %d rows, want 0", len(rows2))
	}
}

// TestScanTrainedModelStructure trains on synthetic trades and checks the
// scan output shape: top-N rows, sorted desc, probabilities in range.
// Structural only (train init is random), so it never flakes.
func TestScanTrainedModelStructure(t *testing.T) {
	initSymbolEncodings()
	var feats []mlFeature
	var labels []float64
	for i := 0; i < 40; i++ {
		dte := 7 + (i % 5 * 9) // 7..43
		feats = append(feats, mlFeature{
			DTE: float64(dte), IV: 0.2 + float64(i%3)*0.1,
			Trend: 1, VolRegime: 0,
			Strategy: 2, Symbol: 0,
		})
		if dte >= 25 {
			labels = append(labels, 1.0)
		} else {
			labels = append(labels, 0.0)
		}
	}
	model := trainLogistic(feats, labels, 0.05, 300, 0.001)
	rows := scanMLCombinations(&model,
		[]string{"Si", "RI"},
		[]string{"bull_put_spread", "bear_call_spread", "iron_condor"},
		[]int{7, 14, 21, 30, 45}, []float64{15, 20, 30, 45, 60},
		[]string{"BULLISH", "BEARISH", "SIDEWAYS"}, []string{"IV>HV", "IV<HV", "neutral"},
		8)
	if len(rows) != 8 {
		t.Fatalf("got %d rows, want top 8", len(rows))
	}
	for i, r := range rows {
		if r.WinProb < 0 || r.WinProb > 1 {
			t.Fatalf("row %d prob %v out of range", i, r.WinProb)
		}
		if i > 0 && rows[i-1].WinProb < r.WinProb {
			t.Fatal("rows not sorted by win prob desc")
		}
		if r.Confidence != "HIGH" && r.Confidence != "MEDIUM" && r.Confidence != "LOW" {
			t.Fatalf("row %d bad confidence %q", i, r.Confidence)
		}
	}
}

func TestScoreMLFeatureBands(t *testing.T) {
	model := testScanModel()
	hot := mlFeature{DTE: 45}
	prob, conf := scoreMLFeature(model, hot)
	if prob < 0.7 || conf != "HIGH" {
		t.Fatalf("hot features: prob=%v conf=%q, want HIGH", prob, conf)
	}
	cold := mlFeature{DTE: 0}
	prob, conf = scoreMLFeature(model, cold)
	if prob != 0.5 || conf != "LOW" {
		t.Fatalf("neutral features: prob=%v conf=%q, want 0.5/LOW", prob, conf)
	}
}
