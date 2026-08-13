package quant

import (
	"math"
)

type ArbitrageOpportunity struct {
	Symbol          string  `json:"symbol"`
	SpotPrice       float64 `json:"spot_price"`
	Strike          float64 `json:"strike"`
	DaysToExp       float64 `json:"days_to_exp"`
	ActualCallPrice float64 `json:"actual_call_price"`
	ActualPutPrice  float64 `json:"actual_put_price"`
	TheoreticalDiff float64 `json:"theoretical_diff"`
	ActualDiff      float64 `json:"actual_diff"`
	Spread          float64 `json:"spread"`
	Strategy        string  `json:"strategy"` // "Conversion", "Reversal", or "No Arbitrage"
	ExpectedProfit  float64 `json:"expected_profit"`
}

// CheckPutCallParity проверяет наличие арбитражного спрэда
func CheckPutCallParity(symbol string, spot, strike, days, callPrice, putPrice, riskFree float64) ArbitrageOpportunity {
	t := days / 365.0
	discountFactor := math.Exp(-riskFree * t)

	// Теоретическая разница: C - P должна равняться (S - K * e^(-rT))
	theoreticalDiff := spot - (strike * discountFactor)
	actualDiff := callPrice - putPrice

	spread := actualDiff - theoreticalDiff

	strategy := "No Arbitrage"
	expectedProfit := 0.0

	// Если Call переоценен относительно Put -> Продаем Call, покупаем Put и спот (Conversion)
	if spread > 10.0 { // Порог $10 с учетом комиссий
		strategy = "Conversion (Sell Call, Buy Put & Spot)"
		expectedProfit = math.Round(spread*100) / 100
	} else if spread < -10.0 { // Если Put переоценен -> Reversal
		strategy = "Reversal (Buy Call, Sell Put & Short Spot)"
		expectedProfit = math.Round(math.Abs(spread)*100) / 100
	}

	return ArbitrageOpportunity{
		Symbol:          symbol,
		SpotPrice:       math.Round(spot*100) / 100,
		Strike:          math.Round(strike*100) / 100,
		DaysToExp:       days,
		ActualCallPrice: callPrice,
		ActualPutPrice:  putPrice,
		TheoreticalDiff: math.Round(theoreticalDiff*100) / 100,
		ActualDiff:      math.Round(actualDiff*100) / 100,
		Spread:          math.Round(spread*100) / 100,
		Strategy:        strategy,
		ExpectedProfit:  expectedProfit,
	}
}