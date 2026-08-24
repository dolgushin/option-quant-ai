package main

// Statistics module: full-account trade analytics for the «Статистика» tab.
// Includes every trade (spreads included) and buckets results by strategy,
// symbol, entry trend, entry vol regime and DTE window so the forecast module
// and the trader can see which strategies worked in which conditions.

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"

	"option-quant-ai/quant"
)

type statsRow struct {
	Key          string  `json:"key"`
	Trades       int     `json:"trades"`
	NetPnl       float64 `json:"net_pnl"`
	Wins         int     `json:"wins"`
	WinRate      float64 `json:"win_rate"` // 0..1
	AvgPnl       float64 `json:"avg_pnl"`
	ProfitFactor float64 `json:"profit_factor"`
	Expectancy   float64 `json:"expectancy"`
	Best         float64 `json:"best"`
	Worst        float64 `json:"worst"`
}

type statsEquityPoint struct {
	Date     string  `json:"date"`
	Equity   float64 `json:"equity"`
	Drawdown float64 `json:"drawdown"` // negative
}

type monthPnl struct {
	Month string  `json:"month"`
	Pnl   float64 `json:"pnl"`
}

type histBucket struct {
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	Count int     `json:"count"`
}

type statsOverview struct {
	Trades        int                `json:"trades"`
	NetPnl        float64            `json:"net_pnl"`
	Wins          int                `json:"wins"`
	Losses        int                `json:"losses"`
	WinRate       float64            `json:"win_rate"`
	ProfitFactor  float64            `json:"profit_factor"`
	Expectancy    float64            `json:"expectancy"`
	AvgWin        float64            `json:"avg_win"`
	AvgLoss       float64            `json:"avg_loss"`
	MaxDrawdown   float64            `json:"max_drawdown"`
	MaxWinStreak  int                `json:"max_win_streak"`
	MaxLossStreak int                `json:"max_loss_streak"`
	SharpeTrade   float64            `json:"sharpe_trade"`
	Equity        []statsEquityPoint `json:"equity"`
	Monthly       []monthPnl         `json:"monthly"`
	Histogram     []histBucket       `json:"histogram"`
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// computeStatsOverview is a pure aggregator over closed trades.
func computeStatsOverview(trades []quant.Trade) *statsOverview {
	o := &statsOverview{Trades: len(trades)}
	if len(trades) == 0 {
		return o
	}
	sorted := make([]quant.Trade, len(trades))
	copy(sorted, trades)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ClosedAt.Before(sorted[j].ClosedAt) })

	var sumWin, sumLoss, sumPnl, sumSq float64
	equity := 0.0
	peak := 0.0
	winStreak, lossStreak := 0, 0
	monthly := map[string]float64{}

	for _, t := range sorted {
		pnl := t.RealizedPnL
		o.NetPnl += pnl
		sumPnl += pnl
		sumSq += pnl * pnl
		equity += pnl
		if equity > peak {
			peak = equity
		}
		dd := equity - peak
		if dd < o.MaxDrawdown {
			o.MaxDrawdown = dd
		}
		if pnl > 0 {
			o.Wins++
			sumWin += pnl
			winStreak++
			lossStreak = 0
			if winStreak > o.MaxWinStreak {
				o.MaxWinStreak = winStreak
			}
		} else if pnl < 0 {
			o.Losses++
			sumLoss += -pnl
			lossStreak++
			winStreak = 0
			if lossStreak > o.MaxLossStreak {
				o.MaxLossStreak = lossStreak
			}
		}
		o.Equity = append(o.Equity, statsEquityPoint{
			Date: t.ClosedAt.Format("2006-01-02"), Equity: round2(equity), Drawdown: round2(dd),
		})
		monthly[t.ClosedAt.Format("2006-01")] += pnl
	}

	o.NetPnl = round2(o.NetPnl)
	o.WinRate = round2(float64(o.Wins) / float64(o.Trades))
	o.Expectancy = round2(sumPnl / float64(o.Trades))
	o.AvgWin = round2(sumWin / math.Max(float64(o.Wins), 1))
	o.AvgLoss = round2(sumLoss / math.Max(float64(o.Losses), 1))
	if sumLoss > 0 {
		o.ProfitFactor = round2(sumWin / sumLoss)
	} else if sumWin > 0 {
		o.ProfitFactor = 99
	}
	mean := sumPnl / float64(o.Trades)
	variance := sumSq/float64(o.Trades) - mean*mean
	if variance > 0 {
		o.SharpeTrade = round2(mean / math.Sqrt(variance))
	}

	for m, pnl := range monthly {
		o.Monthly = append(o.Monthly, monthPnl{Month: m, Pnl: round2(pnl)})
	}
	sort.Slice(o.Monthly, func(i, j int) bool { return o.Monthly[i].Month < o.Monthly[j].Month })

	// PnL histogram: ~12 buckets over the observed range.
	lo, hi := math.MaxFloat64, -math.MaxFloat64
	for _, t := range sorted {
		lo = math.Min(lo, t.RealizedPnL)
		hi = math.Max(hi, t.RealizedPnL)
	}
	if hi > lo {
		const nb = 12
		width := (hi - lo) / nb
		o.Histogram = make([]histBucket, nb)
		for i := range o.Histogram {
			o.Histogram[i] = histBucket{From: round2(lo + float64(i)*width), To: round2(lo + float64(i+1)*width)}
		}
		for _, t := range sorted {
			idx := int((t.RealizedPnL - lo) / width)
			if idx >= nb {
				idx = nb - 1
			}
			o.Histogram[idx].Count++
		}
	}
	return o
}

// computeBreakdown groups trades by an arbitrary key function.
func computeBreakdown(trades []quant.Trade, keyFn func(quant.Trade) string) []statsRow {
	agg := map[string]*statsRow{}
	for _, t := range trades {
		k := keyFn(t)
		if k == "" {
			k = "нет данных"
		}
		r := agg[k]
		if r == nil {
			r = &statsRow{Key: k}
			agg[k] = r
		}
		r.Trades++
		r.NetPnl += t.RealizedPnL
		if t.RealizedPnL > 0 {
			r.Wins++
		}
		r.Best = math.Max(r.Best, t.RealizedPnL)
		r.Worst = math.Min(r.Worst, t.RealizedPnL)
	}
	out := make([]statsRow, 0, len(agg))
	for _, r := range agg {
		var sumWin, sumLoss float64
		for _, t := range trades {
			k := keyFn(t)
			if k == "" {
				k = "нет данных"
			}
			if k == r.Key {
				if t.RealizedPnL > 0 {
					sumWin += t.RealizedPnL
				} else {
					sumLoss += -t.RealizedPnL
				}
			}
		}
		r.NetPnl = round2(r.NetPnl)
		r.WinRate = round2(float64(r.Wins) / math.Max(float64(r.Trades), 1))
		r.AvgPnl = round2(r.NetPnl / math.Max(float64(r.Trades), 1))
		r.Expectancy = r.AvgPnl
		if sumLoss > 0 {
			r.ProfitFactor = round2(sumWin / sumLoss)
		} else if sumWin > 0 {
			r.ProfitFactor = 99
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NetPnl > out[j].NetPnl })
	return out
}

func dteBucket(dte int) string {
	switch {
	case dte <= 0:
		return ""
	case dte <= 7:
		return "1 нед (≤7)"
	case dte <= 21:
		return "2–3 нед (8–21)"
	case dte <= 45:
		return "1–1.5 мес (22–45)"
	default:
		return "> 45"
	}
}

// statsOverviewHandler returns full-account aggregates.
// URL: /api/v2/stats/overview
func statsOverviewHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(computeStatsOverview(quant.GetTrades()))
}

// statsBreakdownHandler groups closed trades by a dimension.
// URL: /api/v2/stats/breakdown?dim=strategy|symbol|trend|vol|dte
func statsBreakdownHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	dim := r.URL.Query().Get("dim")
	trades := quant.GetTrades()
	var rows []statsRow
	switch dim {
	case "symbol":
		rows = computeBreakdown(trades, func(t quant.Trade) string { return t.Symbol })
	case "trend":
		rows = computeBreakdown(trades, func(t quant.Trade) string { return t.TrendAtEntry })
	case "vol":
		rows = computeBreakdown(trades, func(t quant.Trade) string { return t.VolRegime })
	case "dte":
		rows = computeBreakdown(trades, func(t quant.Trade) string { return dteBucket(t.DTEAtEntry) })
	default:
		rows = computeBreakdown(trades, func(t quant.Trade) string { return t.Strategy })
	}
	if rows == nil {
		rows = []statsRow{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"dim": dim, "rows": rows})
}
