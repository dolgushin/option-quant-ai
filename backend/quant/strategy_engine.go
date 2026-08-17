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
	StrategyType   string   `json:"strategy_type"`   // maps to /api/v1/strategy/build
	RealTheta      float64  `json:"real_theta"`      // live theta, filled by handler
	RealMaxProfit  float64  `json:"real_max_profit"` // live, filled by handler
	RealMaxLoss    float64  `json:"real_max_loss"`   // live, filled by handler
	RealMargin     float64  `json:"real_margin"`     // live GO, filled by handler
	RealSpot       float64  `json:"real_spot"`       // live spot, filled by handler
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
			ExpectedTheta: 0,
			MaxProfit:    "Net premium received",
			RiskProfile:  "Defined risk, max loss at wings",
			ExitRule:     "Take profit at 50% max profit or close 30 DTE before expiration.",
			StrategyType: "iron_condor",
		})

		recs = append(recs, StrategyRecommendation{
			StrategyName: "Iron Butterfly (Железная бабочка)",
			Regime:       string(regime),
			Suitability:  "Medium (ATM premium collection)",
			TargetLegs:   []string{"Sell ATM Put", "Buy OTM Put", "Sell ATM Call", "Buy OTM Call"},
			ExpectedTheta: 0,
			MaxProfit:    "Net premium at ATM strike",
			RiskProfile:  "Defined risk, high gamma sensitivity near expiration",
			ExitRule:     "Time stop at 30% DTE left. Take profit at 20% max profit.",
			StrategyType: "iron_butterfly",
		})
	} else {
		recs = append(recs, StrategyRecommendation{
			StrategyName: "Long Volatility & Gamma Scalping",
			Regime:       string(regime),
			Suitability:  "High (Explosive movement expected)",
			TargetLegs:   []string{"Buy ATM Straddle / Strangle", "Delta Hedge via Futures"},
			ExpectedTheta: 0,
			MaxProfit:    "Unlimited on breakout",
			RiskProfile:  "Theta decay cost, mitigated by gamma scalping",
			ExitRule:     "Reset delta when price moves by calculated Move Step.",
			StrategyType: "long_volatility",
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

type RollingAdvice struct {
	StrategyType      string `json:"strategy_type"`
	Condition         string `json:"condition"`
	RecommendedAction string `json:"recommended_action"`
	Details           string `json:"details"`
}

// EvaluateVerticalSpreads decides whether Call Spread or Put Spread is more advantageous based on IV and outlook
func EvaluateVerticalSpreads(iv, hv float64, outlook string) StrategyRecommendation {
	if outlook == "BULLISH" {
		if iv > hv*1.2 {
			return StrategyRecommendation{
				StrategyName: "Bull Put Spread (Credit Put Spread)",
				Regime:       "High IV / Bullish",
				Suitability:  "Выгоднее: высокая IV позволяет продать дорогой пут и купить защиту дешевле, собирая временной распад.",
				TargetLegs:   []string{"Sell OTM Put", "Buy Far OTM Put"},
				ExpectedTheta: 0,
				MaxProfit:    "Net Premium Collected",
				RiskProfile:  "Defined risk (Strike Width - Premium)",
				ExitRule:     "Take profit at 50% max profit. Roll if tested.",
				StrategyType: "bull_put_spread",
			}
		} else {
			return StrategyRecommendation{
				StrategyName: "Bull Call Spread (Debit Call Spread)",
				Regime:       "Low/Normal IV / Bullish",
				Suitability:  "Выгоднее: IV низкая, покупка колл-спреда дешевле по премии при ожидании роста.",
				TargetLegs:   []string{"Buy ATM/OTM Call", "Sell Higher OTM Call"},
				ExpectedTheta: 0,
				MaxProfit:    "Spread Width - Net Debit",
				RiskProfile:  "Limited to net debit paid",
				ExitRule:     "Take profit at 70% max profit.",
				StrategyType: "bull_call_spread",
			}
		}
	} else { // BEARISH
		if iv > hv*1.2 {
			return StrategyRecommendation{
				StrategyName: "Bear Call Spread (Credit Call Spread)",
				Regime:       "High IV / Bearish",
				Suitability:  "Выгоднее: высокая IV идеальна для продажи колл-спреда (сбор премии за счет падения или стояния рынка).",
				TargetLegs:   []string{"Sell OTM Call", "Buy Far OTM Call"},
				ExpectedTheta: 0,
				MaxProfit:    "Net Premium Collected",
				RiskProfile:  "Defined risk",
				ExitRule:     "Take profit at 50% max profit.",
				StrategyType: "bear_call_spread",
			}
		} else {
			return StrategyRecommendation{
				StrategyName: "Bear Put Spread (Debit Put Spread)",
				Regime:       "Low/Normal IV / Bearish",
				Suitability:  "Выгоднее: покупка пут-спреда при недорогой волатильности для защиты от падения.",
				TargetLegs:   []string{"Buy ATM Put", "Sell Lower OTM Put"},
				ExpectedTheta: 0,
				MaxProfit:    "Spread Width - Net Debit",
				RiskProfile:  "Limited to net debit paid",
				ExitRule:     "Take profit at 70% max profit.",
				StrategyType: "bear_put_spread",
			}
		}
	}
}

// GetSpreadRollingAdvice returns rolling and transformation options when spread hits Decision Point (TPR)
func GetSpreadRollingAdvice(marketDirection string, drawdownPct float64) RollingAdvice {
	if drawdownPct >= 30.0 {
		switch marketDirection {
		case "BULLISH":
			return RollingAdvice{
				StrategyType:      "Vertical Spread Rolling",
				Condition:         "Цена пошла против позиции (достигнута точка TPR при ожидании роста)",
				RecommendedAction: "Реконструкция в Лестницу (Ladder) или покупка около-ATM коллов",
				Details:           "Сохраняем восходящий взгляд, но снижаем дельта-риск путем перестройки в асимметричный спред.",
			}
		case "BEARISH":
			return RollingAdvice{
				StrategyType:      "Vertical Spread Rolling",
				Condition:         "Достигнута точка TPR при ожидании падения",
				RecommendedAction: "Трансформация в Backspread с покупкой Put",
				Details:           "Участие в сильном нисходящем движении с дополнительным хеджированием волатильности.",
			}
		default:
			return RollingAdvice{
				StrategyType:      "Vertical Spread Rolling",
				Condition:         "Рынок ушел в боковик (Флэт)",
				RecommendedAction: "Продажа противоположных ног (трансформация в Short Strangle / Condor)",
				Details:           "Использование накопленного тета-распада для компенсации просадки по направлению.",
			}
		}
	}
	return RollingAdvice{
		StrategyType:      "Vertical Spread",
		Condition:         "Позиция стабильна",
		RecommendedAction: "Удержание до достижения 70% профита или сработки Stop-Loss",
		Details:           "Правило роллирования: роллировать ТОЛЬКО если сохраняется исходный Edge. Запрещено увеличивать риск ради избежания фиксации убытка.",
	}
}
