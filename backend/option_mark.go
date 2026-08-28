package main

// Hybrid mark-to-model for option legs. Live narrow books are marked at the
// bid/ask mid (the market is right); dead or absurdly wide books are marked at
// Black-Scholes fair value using a series IV estimated from the liquid part of
// the same chain — the same approach the official MOEX constructor applies.
// This keeps P&L physical (a 500-wide spread can never "move to" 2400) on
// illiquid series where a stale two-sided quote is pure noise.

import (
	"sort"
	"sync"
	"time"

	"option-quant-ai/quant"
)

const (
	// markLiveSpreadPct is the widest book spread (offer-bid, relative to mid)
	// that is still treated as a live, trustworthy quote. Liquid front-month
	// ATM options on MOEX trade well inside 25%; wide OTM tails are ignored.
	markLiveSpreadPct = 0.25

	// markSeriesIVTTL bounds how often the series IV estimate refreshes.
	// The estimate itself polls the chain (several ISS rows), so it is cached.
	markSeriesIVTTL = 60 * time.Second
)

var (
	seriesIVCache = map[string]seriesIVEntry{}
	seriesIVMu    sync.Mutex
)

type seriesIVEntry struct {
	IV     float64
	Cached time.Time
}

// quoteIsLive decides whether a top-of-book quote is usable for mark-to-market:
// two-sided, not stale, and not absurdly wide relative to its own mid.
func quoteIsLive(q optionQuoteEx) bool {
	if q.Price <= 0 || q.Bid <= 0 || q.Offer < q.Bid {
		return false
	}
	mid := (q.Bid + q.Offer) / 2
	if mid <= 0 {
		return false
	}
	if (q.Offer-q.Bid)/mid > markLiveSpreadPct {
		return false
	}
	return !quoteIsStale(q.Updated, q.Src)
}

// seriesIVForExpiry estimates one "fair" implied volatility for an option
// series: the median IV of the liquid, near-the-money strikes in its chain.
// Falls back to realized volatility of the underlying, then 0.30.
func seriesIVForExpiry(symbol, expiry string) float64 {
	key := symbol + "|" + expiry
	seriesIVMu.Lock()
	if e, ok := seriesIVCache[key]; ok && e.Cached.Add(markSeriesIVTTL).After(time.Now()) {
		seriesIVMu.Unlock()
		return e.IV
	}
	seriesIVMu.Unlock()

	spot, _ := getSpotPrice(symbol)
	if spot <= 0 {
		return 0.30
	}
	dte := dteInDays(expiry, time.Now())
	if dte <= 0 {
		dte = 30
	}
	t := float64(dte) / 365.0

	chain := moexOptionsForAsset(symbol, expiry)
	samples := make(chan float64, len(chain))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, o := range chain {
		if o.Strike <= 0 {
			continue
		}
		// Near-the-money only: the mid of the chain is where the book is real.
		mn := o.Strike / spot
		if mn < 0.9 || mn > 1.1 {
			continue
		}
		o := o
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			q, ok := cachedOptionQuoteEx(o.SecID)
			if !ok || q.Bid <= 0 || q.Offer < q.Bid {
				return
			}
			mid := (q.Bid + q.Offer) / 2
			if mid <= 0 || (q.Offer-q.Bid)/mid > markLiveSpreadPct {
				return // dead book — not informative for vol
			}
			iv := quant.ImpliedVolatility(o.IsCall, mid, spot, o.Strike, t, 0.16)
			if iv > 0.02 && iv <= 3 {
				samples <- iv
			}
		}()
	}
	wg.Wait()
	close(samples)

	ivs := make([]float64, 0, len(samples))
	for v := range samples {
		ivs = append(ivs, v)
	}

	iv := 0.30
	if len(ivs) > 0 {
		sort.Float64s(ivs)
		iv = ivs[len(ivs)/2]
	} else if rv := realizedVolForSymbol(symbol); rv > 0.02 && rv <= 3 {
		iv = rv
	}

	seriesIVMu.Lock()
	seriesIVCache[key] = seriesIVEntry{IV: iv, Cached: time.Now()}
	seriesIVMu.Unlock()
	return iv
}

// optionMark returns the price an option leg should be marked at: the live mid
// when the book is trustworthy, otherwise BS fair value at the series IV. A
// single-sided clean last trade without a stale book is also usable.
func optionMark(secid string, isCall bool, strike, spot float64, tYears float64, symbol, expiry string) float64 {
	p, _ := optionMarkWithSrc(secid, isCall, strike, spot, tYears, symbol, expiry)
	return p
}

// optionMarkWithSrc is optionMark plus the provenance of the mark:
// "mid" — live two-sided book, "last" — clean single-sided trade, "theo" —
// Black-Scholes fair value at the series IV, "none" — nothing usable.
func optionMarkWithSrc(secid string, isCall bool, strike, spot float64, tYears float64, symbol, expiry string) (float64, string) {
	if q, ok := cachedOptionQuoteEx(secid); ok {
		if quoteIsLive(q) {
			return q.Price, "mid"
		}
		if q.Src == "last" && q.Price > 0 && !quoteIsStale(q.Updated, q.Src) {
			return q.Price, "last"
		}
	}
	if spot > 0 && strike > 0 && tYears > 0 {
		if iv := seriesIVForExpiry(symbol, expiry); iv > 0 {
			return quant.CalculateBlackScholes(isCall, spot, strike, tYears, 0.16, iv).Price, "theo"
		}
	}
	return 0, "none"
}
