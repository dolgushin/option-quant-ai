package main

import (
	"testing"
	"time"
)

func TestOptionMarkQuoteIsLive(t *testing.T) {
	ok := func(q optionQuoteEx) bool { return quoteIsLive(q) }

	// Two-sided, fresh, tight spread → live.
	if !ok(optionQuoteEx{Price: 500, Bid: 460, Offer: 540, Updated: time.Now().Format("15:04:05"), Src: "mid"}) {
		t.Error("tight fresh book must be live")
	}
	// Snearly 25% boundary: a 27% spread is dead.
	if ok(optionQuoteEx{Price: 2375, Bid: 2050, Offer: 2700, Updated: time.Now().Format("15:04:05"), Src: "mid"}) {
		t.Errorf("wide book must not be live: spread=%.0f%%", (2700.0-2050.0)/2375.0*100)
	}
	// Empty/one-sided → dead.
	if ok(optionQuoteEx{Price: 0, Bid: 0, Offer: 0}) {
		t.Error("empty book must be dead")
	}
	if ok(optionQuoteEx{Price: 300, Bid: 300, Offer: 0, Updated: time.Now().Format("15:04:05"), Src: "last"}) {
		t.Error("one-sided book must be dead")
	}
	// Stale timestamp → dead regardless of tightness.
	staleT := time.Now().Add(-2 * time.Hour).Format("15:04:05")
	if ok(optionQuoteEx{Price: 500, Bid: 460, Offer: 540, Updated: staleT, Src: "mid"}) {
		t.Error("stale book must be dead")
	}
}