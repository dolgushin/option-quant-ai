package main

import (
	"math"
	"testing"
	"time"
)

func TestCrParseDeribitExpiry(t *testing.T) {
	exp, ok := crParseDeribitExpiry("26SEP25")
	if !ok {
		t.Fatal("not parsed")
	}
	if exp.Year() != 2025 || exp.Month() != time.September || exp.Day() != 26 {
		t.Fatalf("bad date: %v", exp)
	}
	if _, ok := crParseDeribitExpiry("XX"); ok {
		t.Fatal("should fail")
	}
}

func TestCrParseInstrument(t *testing.T) {
	exp, k, isCall, ok := crParseInstrument("BTC-26SEP25-90000-C")
	if !ok || k != 90000 || !isCall || exp.Day() != 26 {
		t.Fatalf("bad parse: %v %v %v %v", exp, k, isCall, ok)
	}
	_, _, isCall, ok = crParseInstrument("ETH-3OCT25-4000-P")
	if !ok || isCall {
		t.Fatal("put parse failed")
	}
}

func TestCrNormIV(t *testing.T) {
	v, ok := crNormIV(33.4)
	if !ok || math.Abs(v-0.334) > 1e-9 {
		t.Fatalf("percent not normalized: %v", v)
	}
	v, ok = crNormIV(0.33)
	if !ok || math.Abs(v-0.33) > 1e-9 {
		t.Fatalf("fraction broken: %v", v)
	}
	if _, ok := crNormIV(0); ok {
		t.Fatal("zero should fail")
	}
}

func TestCrPickBucket(t *testing.T) {
	now := time.Now()
	gs := []*crExpGroup{
		{DTE: 7.2, Key: "a"},
		{DTE: 31, Key: "b"},
		{DTE: 200, Key: "c"},
	}
	_ = now
	if g := crPickBucket(gs, 30); g == nil || g.Key != "b" {
		t.Fatal("should pick 31d for 1M")
	}
	if g := crPickBucket(gs, 180); g == nil || g.Key != "c" {
		t.Fatal("should pick 200d for 6M")
	}
}

func TestCrIVRank(t *testing.T) {
	hist := []float64{0.3, 0.35, 0.4, 0.45, 0.5, 0.32, 0.38, 0.42, 0.36, 0.44, 0.5, 0.31}
	r, mn, mx, ok := crIVRank(0.31, hist)
	if !ok || mn != 0.3 || mx != 0.5 {
		t.Fatalf("bad minmax: %v %v %v", r, mn, mx)
	}
	if r > 10 {
		t.Fatalf("near-low rank should be small: %v", r)
	}
}

func TestCrBuildSummary(t *testing.T) {
	s := crBuildSummary("BTC", 0, -0.05, 6.5, true, 0.05, 0.0, 75500, 81500)
	if len(s.Bullets) != 5 {
		t.Fatalf("need 5 bullets, got %d", len(s.Bullets))
	}
	if len(s.Detail) != 2 {
		t.Fatal("need 2 detail paragraphs")
	}
}
