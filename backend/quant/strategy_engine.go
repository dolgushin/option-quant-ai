package quant

import (
	"fmt"
	"math"
)

type MarketRegime string

const (
	RegimeCalm       MarketRegime = "Calm (Low IV / Rangebound)"
	RegimeStress     MarketRegime = "Stress (High IV / Tail Risk)"
	RegimeTrend      MarketRegime = "Trend (Directional Breakout)"
	RegimeHighTheta  MarketRegime = "High IV (Theta Harvesting)"
)

type StrategyRecommendation struct {
	StrategyName   string   `json:"strategy_name"`
	Regime         string   `json:"market_regime"`
	Suitability    string   `json:"suitability"`
	TargetLegs     []string `json:"target_legs"`
	ExpectedTheta  float64  `json:"expected_theta"`
	MaxProfit      string   `json:"max_profit"`
	RiskProfile    string   `json:"risk_profile"`
	ExitRule       string   `json:"exit_rule"`
}

type ExitAdvice struct {
	TriggerType string `json:"trigger_type"` // "DTE", "DELTA_DRIFT", "VEGA_PROFIT", "NONE"
	Severity    string `json:"severity"`     // "INFO", "WARNING", "CRITICAL"
	Message     string `json:"message"`
	Action      string `json:"action"`
}

// ClassifyMarketRegime classifies market conditions based on IV and recent price movement
func ClassifyMarketRegime(iv, hv float64, isTrending bool) MarketRegime {
	if iv > hv*1.3 {
		if isTrending {
			return RegimeStress
		}
		return RegimeHighTheta
	}
	if isTrending {
		return RegimeTrend
	}
	return RegimeCalm
}

// GenerateStrategyRecommendations generates structured option strategies based on regime
func GenerateStrategyRecommendations(iv, hv, spot float64) []StrategyRecommendation {
	regime := ClassifyMarketRegime(iv, hv, false)
	var recs []StrategyRecommendation

	if regime == RegimeHighTheta || regime == RegimeCalm {
		recs = append(recs, StrategyRecommendation{
			StrategyName: "Iron Condor (Железный кондор)",
			Regime:       string(regime),
			Suitability:  "High (Ideal for Theta collection in rangebound markets)",
			TargetLegs:   []string{"Sell OTM Put", "Buy Far OTM Put", "Sell OTM Call", "Buy Far OTM Call"},
			ExpectedTheta: 1250.0,
			MaxProfit:    "Net premium received",
			RiskProfile:  "Defined risk, max loss at wings",
			ExitRule:     "Take profit at 50% max profit or close 30 DTE before expiration.",
		})

		recs = append(recs, StrategyRecommendation{
			StrategyName: "Iron Butterfly (Железная бабочка)",
			Regime:       string(regime),
			Suitability:  "Medium (ATM premium collection)",
			TargetLegs:   []string{"Sell ATM Put", "Buy OTM Put", "Sell ATM Call", "Buy OTM Call"},
			ExpectedTheta: 1800.0,
			MaxProfit:    "Net premium at ATM strike",
			RiskProfile:  "Defined risk, high gamma sensitivity near expiration",
			ExitRule:     "Time stop at 30% DTE left. Take profit at 20% max profit.",
		})
	} else {
		recs = append(recs, StrategyRecommendation{
			StrategyName: "Long Volatility & Gamma Scalping",
			Regime:       string(regime),
			Suitability:  "High (Explosive movement expected)",
			TargetLegs:   []string{"Buy ATM Straddle / Strangle", "Delta Hedge via Futures"},
			ExpectedTheta: -450.0,
			MaxProfit:    "Unlimited on breakout",
			RiskProfile:  "Theta decay cost, mitigated by gamma scalping",
			ExitRule:     "Reset delta when price moves by calculated Move Step.",
		})
	}

	return recs
}

// CalculateGammaScalpingStep computes move step for gamma scalping: sqrt(2.8 * Theta / Gamma)
func CalculateGammaScalpingStep(theta, gamma float64) float64 {
	if gamma <= 0 || theta >= 0 {
		return 0.0
	}
	// theta is negative for long options
	step := math.Sqrt((2.8 * math.Abs(theta)) / gamma)
	return math.Round(step*100) / 100
}

// AnalyzeExitTriggers analyzes active position telemetry for exit advice
func AnalyzeExitTriggers(dte float64, delta float64, profitPct float64) ExitAdvice {
	if dte <= 10 {
		return ExitAdvice{
			TriggerType: "DTE",
			Severity:    "CRITICAL",
			Message:     "Осталось менее 10 дней до экспирации. Экстремальный гамма-риск.",
			Action:      "CLOSE_OR_ROLL",
		}
	}
	if math.Abs(delta) >= 0.5 {
		return ExitAdvice{
			TriggerType: "DELTA_DRIFT",
			Severity:    "WARNING",
			Message:     fmt.Sprintf("Дельта позиции сместилась до критического уровня (Δ = %.2f). Нарушена нейтральность.", delta),
			Action:      "EXECUTE_DELTA_HEDGE",
		}
	}
	if profitPct >= 70.0 {
		return ExitAdvice{
			TriggerType: "VEGA_PROFIT",
			Severity:    "INFO",
			Message:     fmt.Sprintf("Достигнут целевой профит %.0f%% от максимума.", profitPct),
			Action:      "TAKE_PROFIT_AND_CLOSE",
		}
	}

	return ExitAdvice{
		TriggerType: "NONE",
		Severity:    "INFO",
		Message:     "Позиция в пределах плановых параметров риска и тета-распада.",
		Action:      "HOLD",
	}
}
