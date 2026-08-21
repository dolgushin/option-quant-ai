package main

import (
	"testing"
	"time"
)

// Regression: a weekly series inside a quarter month must not demote its
// quarterly date (Sep weekly + Sep quarterly -> W,W,Q), and isolated dates
// are monthly unless the month is a quarter-end (Q).
func TestClassifyExpiryWMQ(t *testing.T) {
	dates := []string{"2026-08-27", "2026-09-03", "2026-09-17", "2026-10-15", "2026-11-19", "2026-12-17"}
	want := []string{"W", "W", "Q", "M", "M", "Q"}
	var all []time.Time
	for _, d := range dates {
		tm, _ := time.Parse("2006-01-02", d)
		all = append(all, tm)
	}
	for i, d := range dates {
		got := seriesTypeCode(classifyExpiry(d, all))
		if got != want[i] {
			t.Fatalf("%s = %s, want %s", d, got, want[i])
		}
	}
}
