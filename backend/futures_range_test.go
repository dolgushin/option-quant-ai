package main

import (
	"math"
	"testing"
	"time"
)

func TestNormalizeRangeSymbol(t *testing.T) {
	for in, want := range map[string]string{"si": "Si", "SI": "Si", "Si": "Si", "ed": "ED", "ri": "RI", "CR": "CR"} {
		if got := normalizeRangeSymbol(in); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRangeNumber(t *testing.T) {
	if v := parseRangeNumber(1.1608); math.Abs(v-1.1608) > 1e-9 {
		t.Fatalf("float64 parse = %v", v)
	}
	if v := parseRangeNumber("1,1608"); math.Abs(v-1.1608) > 1e-9 {
		t.Fatalf("comma string parse = %v", v)
	}
	if v := parseRangeNumber(" 88014 "); v != 88014 {
		t.Fatalf("int string parse = %v", v)
	}
	if v := parseRangeNumber(nil); v != 0 {
		t.Fatalf("nil parse = %v", v)
	}
}

func TestComputeRangeATRConstant(t *testing.T) {
	// Flat 100-pt daily ranges → ATR must converge to 100.
	candles := make([]rangeOHLC, 20)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range candles {
		candles[i] = rangeOHLC{
			Date: base.AddDate(0, 0, i), Open: 1000, High: 1050, Low: 950, Close: 1000,
		}
	}
	atr := computeRangeATR(candles, 14)
	if atr[12] != 0 {
		t.Fatalf("warming up atr[12] = %v, want 0", atr[12])
	}
	if math.Abs(atr[13]-100) > 1e-9 {
		t.Fatalf("atr[13] = %v, want 100", atr[13])
	}
	if math.Abs(atr[19]-100) > 1e-9 {
		t.Fatalf("atr[19] = %v, want 100", atr[19])
	}
}

func TestComputeRangeATRTrueRange(t *testing.T) {
	// Gap up: TR must include the gap, not just high-low.
	candles := []rangeOHLC{
		{Date: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), High: 110, Low: 100, Close: 105},
		{Date: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), High: 120, Low: 115, Close: 118},
	}
	atr := computeRangeATR(candles, 2)
	// TRs: 10, max(5, |120-105|=15, |115-105|=10) = 15 → avg 12.5.
	if math.Abs(atr[1]-12.5) > 1e-9 {
		t.Fatalf("atr = %v, want 12.5", atr[1])
	}
	if len(candles) < 14 && len(computeRangeATR(candles[:1], 14)) != 1 {
		t.Fatalf("short series must return zero-filled slice")
	}
}

func TestBuildRangeDaysPct(t *testing.T) {
	candles := []rangeOHLC{
		{Date: time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), Open: 85900, High: 86065, Low: 85512, Close: 85834},
	}
	days := buildRangeDays("Si", "SiZ6", candles, []float64{1106})
	if len(days) != 1 {
		t.Fatalf("want 1 day, got %d", len(days))
	}
	d := days[0]
	if d.Range != 553 {
		t.Fatalf("range = %v, want 553", d.Range)
	}
	if d.Change != -66 {
		t.Fatalf("change = %v, want -66", d.Change)
	}
	if math.Abs(d.RangePctATR-50) > 0.1 {
		t.Fatalf("pct = %v, want ~50", d.RangePctATR)
	}
	ed := buildRangeDays("ED", "EDZ6", []rangeOHLC{
		{Date: time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), Open: 1.1477, High: 1.1487, Low: 1.1447, Close: 1.1483},
	}, []float64{0.005})
	if math.Abs(ed[0].Range-0.004) > 1e-9 {
		t.Fatalf("ED range = %v, want 0.004", ed[0].Range)
	}
}

func TestGroupRangeWeeks(t *testing.T) {
	// Mon 2026-09-14 … Fri 2026-09-18.
	days := []rangeDay{
		{Date: "2026-09-14", Symbol: "Si", High: 85992, Low: 84683, Range: 1309, RangePctATR: 100, ATR14: 1309},
		{Date: "2026-09-15", Symbol: "Si", High: 86124, Low: 85173, Range: 951, RangePctATR: 70, ATR14: 1300},
		{Date: "2026-09-16", Symbol: "Si", High: 85988, Low: 85050, Range: 938, RangePctATR: 72, ATR14: 1290},
		{Date: "2026-09-17", Symbol: "Si", High: 86570, Low: 85500, Range: 1070, RangePctATR: 83, ATR14: 1280},
		{Date: "2026-09-18", Symbol: "Si", High: 86065, Low: 85512, Range: 553, RangePctATR: 43, ATR14: 1270},
		{Date: "2026-09-21", Symbol: "Si", High: 86284, Low: 84590, Range: 1694, RangePctATR: 130, ATR14: 1260},
	}
	weeks := groupRangeWeeks(days)
	if len(weeks) != 2 {
		t.Fatalf("weeks = %d, want 2", len(weeks))
	}
	w := weeks[0]
	if w.WeekStart != "2026-09-14" || w.Days != 5 {
		t.Fatalf("week0 = %+v", w)
	}
	if w.SumRange != 1309+951+938+1070+553 {
		t.Fatalf("sum = %v", w.SumRange)
	}
	if w.WeekRange != 86570-84683 {
		t.Fatalf("week range = %v", w.WeekRange)
	}
	if weeks[1].WeekStart != "2026-09-21" || weeks[1].Days != 1 {
		t.Fatalf("week1 = %+v", weeks[1])
	}
}

func TestRangeWeekMonday(t *testing.T) {
	// Monday 2026-09-14 stays, Sunday 2026-09-20 folds back to 09-14.
	mon := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if got := rangeWeekMonday(mon).Format("2006-01-02"); got != "2026-09-14" {
		t.Fatalf("monday = %s", got)
	}
	sun := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if got := rangeWeekMonday(sun).Format("2006-01-02"); got != "2026-09-14" {
		t.Fatalf("sunday folds to %s", got)
	}
}

func TestParseRangeTF(t *testing.T) {
	for in, want := range map[string]int{"5": 5, "10": 10, "60": 60, "": 5, "15": 5, "abc": 5} {
		if got := parseRangeTF(in); got != want {
			t.Fatalf("parseRangeTF(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestBuildIntradayBars(t *testing.T) {
	candles := []rangeOHLC{
		{Date: time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC), Open: 85865, High: 85890, Low: 85840, Close: 85870, Volume: 1200},
		{Date: time.Date(2026, 9, 21, 10, 5, 0, 0, time.UTC), Open: 85870, High: 85900, Low: 85860, Close: 85895, Volume: 900},
	}
	bars := buildIntradayBars("Si", candles)
	if len(bars) != 2 {
		t.Fatalf("want 2 bars, got %d", len(bars))
	}
	if bars[0].Time != "2026-09-21 10:00" {
		t.Fatalf("time = %q", bars[0].Time)
	}
	if bars[0].Range != 50 || bars[1].Range != 40 {
		t.Fatalf("ranges = %v, %v", bars[0].Range, bars[1].Range)
	}
	if avg := intradayAvgRange(bars); avg != 45 {
		t.Fatalf("avg = %v, want 45", avg)
	}
	ed := buildIntradayBars("ED", []rangeOHLC{
		{Date: time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC), Open: 1.1478, High: 1.1485, Low: 1.1475, Close: 1.1480},
	})
	if math.Abs(ed[0].Range-0.001) > 1e-9 {
		t.Fatalf("ED range = %v, want 0.001", ed[0].Range)
	}
	if intradayAvgRange(nil) != 0 {
		t.Fatalf("avg of empty must be 0")
	}
}

func TestAggregateBars(t *testing.T) {
	mk := func(hh, mm int, o, h, l, c, v float64) rangeOHLC {
		return rangeOHLC{
			Date: time.Date(2026, 9, 21, hh, mm, 0, 0, time.UTC),
			Open: o, High: h, Low: l, Close: c, Volume: v, VolumeKnown: true,
		}
	}
	in := []rangeOHLC{
		mk(10, 0, 100, 105, 99, 103, 10),
		mk(10, 1, 103, 107, 102, 106, 20),
		mk(10, 4, 106, 108, 104, 105, 30),
		mk(10, 5, 105, 110, 105, 109, 40),
	}
	bars := aggregateBars(in, 5)
	if len(bars) != 2 {
		t.Fatalf("want 2 buckets, got %d", len(bars))
	}
	b0 := bars[0]
	if b0.Date.Format("15:04") != "10:00" {
		t.Fatalf("bucket0 start = %s", b0.Date.Format("15:04"))
	}
	if b0.Open != 100 || b0.High != 108 || b0.Low != 99 || b0.Close != 105 {
		t.Fatalf("bucket0 OHLC = %+v", b0)
	}
	if b0.Volume != 60 {
		t.Fatalf("bucket0 volume = %v", b0.Volume)
	}
	if bars[1].Open != 105 || bars[1].Close != 109 {
		t.Fatalf("bucket1 = %+v", bars[1])
	}
}

func TestArbAlign(t *testing.T) {
	mk := func(day, hh, mm int, close float64) rangeOHLC {
		return rangeOHLC{
			Date: time.Date(2026, 9, day, hh, mm, 0, 0, time.UTC),
			Open: close, High: close, Low: close, Close: close,
		}
	}
	// Si ~85000, ED ~1.15: after rebase both start at 100.
	a := []rangeOHLC{mk(21, 10, 0, 85000), mk(21, 11, 0, 85850), mk(21, 12, 0, 84150)}
	b := []rangeOHLC{mk(21, 10, 0, 1.15), mk(21, 11, 0, 1.1615), mk(21, 13, 0, 1.17)}
	times, ai, bi, sp := arbAlign(a, b)
	if len(times) != 2 { // 12:00 has no ED bar → inner join drops it
		t.Fatalf("joined = %d, want 2", len(times))
	}
	if ai[0] != 100 || bi[0] != 100 || sp[0] != 0 {
		t.Fatalf("start = %v %v %v, want 100 100 0", ai[0], bi[0], sp[0])
	}
	if math.Abs(ai[1]-101) > 1e-9 || math.Abs(bi[1]-101) > 1e-9 {
		t.Fatalf("+1%% legs = %v %v", ai[1], bi[1])
	}
	if math.Abs(sp[1]) > 1e-9 {
		t.Fatalf("spread of co-moved legs = %v, want 0", sp[1])
	}
	if _, _, _, s := arbAlign(nil, b); s != nil {
		t.Fatalf("empty leg must give nil spread")
	}
}

func TestArbStats(t *testing.T) {
	last, mean, std, z := arbStats([]float64{1, 2, 3})
	if last != 3 || mean != 2 {
		t.Fatalf("last/mean = %v %v", last, mean)
	}
	if math.Abs(std-math.Sqrt(2.0/3)) > 1e-9 {
		t.Fatalf("std = %v", std)
	}
	if math.Abs(z-1/math.Sqrt(2.0/3)) > 1e-6 {
		t.Fatalf("z = %v", z)
	}
	if l, m, s, zz := arbStats(nil); l != 0 || m != 0 || s != 0 || zz != 0 {
		t.Fatalf("empty stats must be zero")
	}
}
