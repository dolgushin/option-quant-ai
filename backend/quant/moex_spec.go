package quant

import (
	"math"
)

type MOEXOptionSpec struct {
	Symbol     string  `json:"symbol"`
	Underlying string  `json:"underlying"` // "Si" or "RI"
	Strike     float64 `json:"strike"`
	Multiplier float64 `json:"multiplier"`
	IsCall     bool    `json:"is_call"`
	TickSize   float64 `json:"tick_size"`
}

// GetMOEXSpec returns contract specifications for Si and RI options on MOEX FORTS
func GetMOEXSpec(symbol string, strike float64, isCall bool) MOEXOptionSpec {
	underlying := "Si"
	multiplier := 1.0
	tickSize := 1.0

	if len(symbol) >= 2 && symbol[:2] == "RI" {
		underlying = "RI"
		multiplier = 2.0 // Standard RTS index option multiplier factor or point value equivalent
		tickSize = 10.0
	} else {
		multiplier = 1.0 // Si options multiplier
		tickSize = 1.0
	}

	return MOEXOptionSpec{
		Symbol:     symbol,
		Underlying: underlying,
		Strike:     strike,
		Multiplier: multiplier,
		IsCall:     isCall,
		TickSize:   tickSize,
	}
}

// CalculateMOEXParitySpread calculates put-call parity for MOEX FORTS options taking into account interest rates (RUONIA / Key Rate)
func CalculateMOEXParitySpread(spot, strike, days, callPrice, putPrice, keyRate float64) (float64, float64, string) {
	t := days / 365.0
	discountFactor := math.Exp(-keyRate * t)

	theoreticalDiff := spot - (strike * discountFactor)
	actualDiff := callPrice - putPrice
	spread := actualDiff - theoreticalDiff

	strategy := "No Arbitrage"
	// Threshold for MOEX FORTS considering exchange fees & slippage (e.g. 50 rubles for Si)
	threshold := 30.0

	if spread > threshold {
		strategy = "Conversion (Sell Call, Buy Put & Underlying Futures)"
	} else if spread < -threshold {
		strategy = "Reversal (Buy Call, Sell Put & Short Underlying Futures)"
	}

	return math.Round(theoreticalDiff*100)/100, math.Round(actualDiff*100)/100, strategy
}
