package quant

import (
	"math"
)

// OptionGreeks содержит рассчитанные показатели опциона
type OptionGreeks struct {
	Price float64 `json:"price"`
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
	Rho   float64 `json:"rho"`
}

// CND - Функция стандартного нормального распределения Cumulative Normal Distribution
func CND(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// NPrime - Плотность вероятности стандартного нормального распределения
func NPrime(x float64) float64 {
	return (1.0 / math.Sqrt(2.0*math.Pi)) * math.Exp(-0.5*x*x)
}

// CalculateBlackScholes рассчитывает стоимость и Греки для Call или Put
func CalculateBlackScholes(isCall bool, S, K, T, r, sigma float64) OptionGreeks {
	if T <= 0 || sigma <= 0 {
		return OptionGreeks{}
	}

	d1 := (math.Log(S/K) + (r+0.5*sigma*sigma)*T) / (sigma * math.Sqrt(T))
	d2 := d1 - sigma*math.Sqrt(T)

	var price, delta, theta float64

	gamma := NPrime(d1) / (S * sigma * math.Sqrt(T))
	vega := S * NPrime(d1) * math.Sqrt(T) / 100.0 // за 1% изменения волатильности

	if isCall {
		price = S*CND(d1) - K*math.Exp(-r*T)*CND(d2)
		delta = CND(d1)
		theta = (-S*NPrime(d1)*sigma/(2.0*math.Sqrt(T)) - r*K*math.Exp(-r*T)*CND(d2)) / 365.0
	} else {
		price = K*math.Exp(-r*T)*CND(-d2) - S*CND(-d1)
		delta = CND(d1) - 1.0
		theta = (-S*NPrime(d1)*sigma/(2.0*math.Sqrt(T)) + r*K*math.Exp(-r*T)*CND(-d2)) / 365.0
	}

	rho := (K * T * math.Exp(-r*T) * CND(d2)) / 100.0
	if !isCall {
		rho = (-K * T * math.Exp(-r*T) * CND(-d2)) / 100.0
	}

	return OptionGreeks{
		Price: math.Round(price*10000) / 10000,
		Delta: math.Round(delta*10000) / 10000,
		Gamma: math.Round(gamma*10000) / 10000,
		Theta: math.Round(theta*10000) / 10000,
		Vega:  math.Round(vega*10000) / 10000,
		Rho:   math.Round(rho*10000) / 10000,
	}
}

// ImpliedVolatility inverts the Black-Scholes price to recover the volatility
// that makes the model match the observed market price. Bisection is used since
// the model price is monotonic in sigma (no risk of Newton overshoot).
func ImpliedVolatility(isCall bool, marketPrice, S, K, T, r float64) float64 {
	if T <= 0 || marketPrice <= 0 {
		return 0
	}

	lo := 0.0001
	hi := 5.0
	priceLo := CalculateBlackScholes(isCall, S, K, T, r, lo).Price
	priceHi := CalculateBlackScholes(isCall, S, K, T, r, hi).Price

	if marketPrice <= priceLo {
		return lo
	}
	if marketPrice >= priceHi {
		return hi
	}

	for i := 0; i < 100; i++ {
		mid := (lo + hi) / 2.0
		p := CalculateBlackScholes(isCall, S, K, T, r, mid).Price
		if math.Abs(p-marketPrice) < 0.0001 {
			return math.Round(mid*10000) / 10000
		}
		if p < marketPrice {
			lo = mid
		} else {
			hi = mid
		}
	}
	return math.Round(((lo+hi)/2.0)*10000) / 10000
}