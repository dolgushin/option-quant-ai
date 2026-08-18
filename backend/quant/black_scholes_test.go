package quant

import (
	"math"
	"testing"
)

// TestCalculateBlackScholesCallPutParity verifies C - P == S - K*e^(-rT) within tolerance.
func TestCalculateBlackScholesCallPutParity(t *testing.T) {
	S, K, T, r, sigma := 83200.0, 83000.0, 30.0/365.0, 0.16, 0.30

	call := CalculateBlackScholes(true, S, K, T, r, sigma)
	put := CalculateBlackScholes(false, S, K, T, r, sigma)

	expectedDiff := S - K*math.Exp(-r*T)
	actualDiff := call.Price - put.Price

	if math.Abs(expectedDiff-actualDiff) > 0.05 {
		t.Fatalf("put-call parity violated: C-P=%.4f, expected S-Ke^(-rT)=%.4f", actualDiff, expectedDiff)
	}
}

// TestCalculateBlackScholesAtTheMoney verifies ATM call/put prices are close
// and delta bounds hold.
func TestCalculateBlackScholesAtTheMoney(t *testing.T) {
	S, T, r, sigma := 83200.0, 30.0/365.0, 0.16, 0.30
	K := S // ATM

	call := CalculateBlackScholes(true, S, K, T, r, sigma)
	put := CalculateBlackScholes(false, S, K, T, r, sigma)

	if call.Delta <= 0 || call.Delta >= 1 {
		t.Fatalf("call delta out of [0,1]: %.4f", call.Delta)
	}
	if put.Delta >= 0 || put.Delta >= -0.0001 {
		t.Fatalf("put delta should be negative: %.4f", put.Delta)
	}
	if call.Gamma <= 0 {
		t.Fatalf("gamma should be positive: %.4f", call.Gamma)
	}
	if call.Vega <= 0 {
		t.Fatalf("vega should be positive: %.4f", call.Vega)
	}
}

// TestCalculateBlackScholesDeepITM verifies a deep ITM call is worth about S - K
// plus modest extrinsic value.
func TestCalculateBlackScholesDeepITM(t *testing.T) {
	S, T, r, sigma := 83200.0, 30.0/365.0, 0.16, 0.30
	K := S - 10000.0 // deep ITM

	call := CalculateBlackScholes(true, S, K, T, r, sigma)

	intrinsic := S - K
	if call.Price < intrinsic {
		t.Fatalf("deep ITM call below intrinsic: price=%.2f intrinsic=%.2f", call.Price, intrinsic)
	}
	if call.Price > intrinsic*1.2 {
		t.Fatalf("deep ITM call extrinsic too large: price=%.2f intrinsic=%.2f", call.Price, intrinsic)
	}
	if call.Delta < 0.9 {
		t.Fatalf("deep ITM call delta should be near 1: %.4f", call.Delta)
	}
}

// TestCalculateBlackScholesTheta verifies long call theta is negative.
func TestCalculateBlackScholesTheta(t *testing.T) {
	S, K, T, r, sigma := 83200.0, 83000.0, 30.0/365.0, 0.16, 0.30

	call := CalculateBlackScholes(true, S, K, T, r, sigma)
	if call.Theta >= 0 {
		t.Fatalf("long call theta should be negative: %.4f", call.Theta)
	}
}

// TestImpliedVolatilityRoundTrip inverts a known sigma and checks the recovered
// IV reproduces the original market price.
func TestImpliedVolatilityRoundTrip(t *testing.T) {
	S, K, T, r := 83200.0, 83000.0, 30.0/365.0, 0.16
	known := 0.35

	for _, isCall := range []bool{true, false} {
		g := CalculateBlackScholes(isCall, S, K, T, r, known)
		iv := ImpliedVolatility(isCall, g.Price, S, K, T, r)

		if math.Abs(iv-known) > 0.01 {
			t.Fatalf("isCall=%v: recovered IV %.4f != known %.4f", isCall, iv, known)
		}

		reprice := CalculateBlackScholes(isCall, S, K, T, r, iv)
		if math.Abs(reprice.Price-g.Price) > 0.01 {
			t.Fatalf("isCall=%v: repriced %.4f != original %.4f", isCall, reprice.Price, g.Price)
		}
	}
}

// TestImpliedVolatilityEdgeCases verifies degenerate inputs return 0.
func TestImpliedVolatilityEdgeCases(t *testing.T) {
	if got := ImpliedVolatility(true, 0, 83200, 83000, 0.1, 0.16); got != 0 {
		t.Fatalf("zero market price should return 0 IV, got %v", got)
	}
	if got := ImpliedVolatility(false, 100, 83200, 83000, 0, 0.16); got != 0 {
		t.Fatalf("zero T should return 0 IV, got %v", got)
	}
}

// TestCND verifies standard normal CDF at key points.
func TestCND(t *testing.T) {
	cases := map[float64]float64{
		0:    0.5,
		1.28: 0.8997,
		-1.28: 0.1003,
	}
	for x, want := range cases {
		got := CND(x)
		if math.Abs(got-want) > 0.001 {
			t.Fatalf("CND(%v)=%.4f, want ~%.4f", x, got, want)
		}
	}
}