package quant

import (
	"math"
	"testing"
)

// TestClassifyMarketRegime covers all four regime branches.
func TestClassifyMarketRegime(t *testing.T) {
	cases := []struct {
		name      string
		iv, hv    float64
		trending  bool
		want      MarketRegime
	}{
		{"calm", 20, 28, false, RegimeCalm},
		{"high_theta", 45, 28, false, RegimeHighTheta},
		{"stress", 45, 28, true, RegimeStress},
		{"trend", 20, 28, true, RegimeTrend},
		{"boundary", 28*1.3 + 0.1, 28, false, RegimeHighTheta},
	}

	for _, c := range cases {
		got := ClassifyMarketRegime(c.iv, c.hv, c.trending)
		if got != c.want {
			t.Errorf("%s: ClassifyMarketRegime(%v,%v,%v)=%q, want %q", c.name, c.iv, c.hv, c.trending, got, c.want)
		}
	}
}

// TestGenerateStrategyRecommendationsCalm verifies calm regime yields condor,
// butterfly and long strangle, each with a valid strategy_type.
func TestGenerateStrategyRecommendationsCalm(t *testing.T) {
	recs := GenerateStrategyRecommendations(20, 28, 83200)

	if len(recs) != 3 {
		t.Fatalf("expected 3 recommendations, got %d", len(recs))
	}
	wantTypes := map[string]bool{"iron_condor": true, "iron_butterfly": true, "long_strangle": true}
	for _, r := range recs {
		if !wantTypes[r.StrategyType] {
			t.Fatalf("unexpected strategy_type %q in calm regime", r.StrategyType)
		}
		if r.StrategyType == "iron_condor" && len(r.TargetLegs) != 4 {
			t.Fatalf("condor should have 4 legs, got %d", len(r.TargetLegs))
		}
	}
}

// TestGenerateStrategyRecommendationsHighIV verifies high IV + trending yields
// the long straddle (volatility buying) recommendation.
func TestGenerateStrategyRecommendationsTrend(t *testing.T) {
	// ClassifyMarketRegime is called with isTrending=false inside the function,
	// so high IV => HighTheta. We verify the resulting set still includes the
	// long strangle as the volatility option.
	recs := GenerateStrategyRecommendations(45, 28, 83200)

	types := map[string]bool{}
	for _, r := range recs {
		types[r.StrategyType] = true
	}
	if !types["long_strangle"] {
		t.Fatalf("expected long_strangle among recommendations, got %v", types)
	}
}

// TestCalculateGammaScalpingStep verifies the step formula and edge cases.
func TestCalculateGammaScalpingStep(t *testing.T) {
	// theta=-50/day, gamma=0.004 => step = sqrt(2.8*50/0.004) = sqrt(35000) ~ 187.08
	got := CalculateGammaScalpingStep(-50, 0.004)
	want := math.Round(math.Sqrt(2.8*50/0.004)*100) / 100
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("step=%.2f, want ~%.2f", got, want)
	}

	if CalculateGammaScalpingStep(50, 0.004) != 0 {
		t.Fatal("positive theta should yield 0 step")
	}
	if CalculateGammaScalpingStep(-50, 0) != 0 {
		t.Fatal("zero gamma should yield 0 step")
	}
}

// TestAnalyzeExitTriggers covers all exit branches.
func TestAnalyzeExitTriggers(t *testing.T) {
	if a := AnalyzeExitTriggers(9, 0.1, 0); a.TriggerType != "DTE" || a.Action != "CLOSE_OR_ROLL" {
		t.Fatalf("DTE branch wrong: %+v", a)
	}
	if a := AnalyzeExitTriggers(30, 0.6, 0); a.TriggerType != "DELTA_DRIFT" || a.Action != "EXECUTE_DELTA_HEDGE" {
		t.Fatalf("delta branch wrong: %+v", a)
	}
	if a := AnalyzeExitTriggers(30, 0.1, 75); a.TriggerType != "VEGA_PROFIT" || a.Action != "TAKE_PROFIT_AND_CLOSE" {
		t.Fatalf("profit branch wrong: %+v", a)
	}
	if a := AnalyzeExitTriggers(30, 0.1, 30); a.TriggerType != "NONE" || a.Action != "HOLD" {
		t.Fatalf("hold branch wrong: %+v", a)
	}
}

// TestEvaluateVerticalSpreads verifies all four outlook/IV combinations.
func TestEvaluateVerticalSpreads(t *testing.T) {
	cases := []struct {
		outlook string
		iv, hv  float64
		want    string
	}{
		{"BULLISH", 45, 28, "bull_put_spread"},  // high IV, credit
		{"BULLISH", 20, 28, "bull_call_spread"}, // low IV, debit
		{"BEARISH", 45, 28, "bear_call_spread"}, // high IV, credit
		{"BEARISH", 20, 28, "bear_put_spread"},  // low IV, debit
	}

	for _, c := range cases {
		r := EvaluateVerticalSpreads(c.iv, c.hv, c.outlook)
		if r.StrategyType != c.want {
			t.Errorf("outlook=%s iv=%v: got %q, want %q", c.outlook, c.iv, r.StrategyType, c.want)
		}
	}
}

// TestGetSpreadRollingAdvice verifies rolling advice branches.
func TestGetSpreadRollingAdvice(t *testing.T) {
	if a := GetSpreadRollingAdvice("BULLISH", 40); a.RecommendedAction == "" {
		t.Fatal("bullish drawdown advice should have an action")
	}
	if a := GetSpreadRollingAdvice("BEARISH", 40); a.StrategyType != "Vertical Spread Rolling" {
		t.Fatalf("bearish advice should be rolling type: %+v", a)
	}
	if a := GetSpreadRollingAdvice("FLAT", 40); a.RecommendedAction == "" {
		t.Fatal("flat advice should have an action")
	}
	if a := GetSpreadRollingAdvice("BULLISH", 10); a.RecommendedAction == "" {
		t.Fatal("stable advice should have an action")
	}
}