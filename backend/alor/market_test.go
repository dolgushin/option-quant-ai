package alor

import (
	"testing"
	"time"
)

// TestNormalizeAlorSecID pins the Alor instrument format at the client
// boundary: month-code futures (SiZ6) rewrite to Si-12.26 (Alor 02.10.2026:
// SiZ6-style codes don't exist — every such request 499s toward the ban),
// everything else passes through so internal matching keeps one spelling.
func TestNormalizeAlorSecID(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range [][2]string{
		{"SiZ6", "Si-12.26"},
		{"SiU6", "Si-09.26"},
		{"SiH7", "Si-03.27"},
		{"RIZ6", "RI-12.26"},
		{"RIU6", "RI-09.26"},
		{"EDZ6", "ED-12.26"},
		{"EDU6", "ED-09.26"},
		{"Si86000BJ6", "Si86000BJ6"},
		{"SBER", "SBER"},
		{"Si-12.26", "Si-12.26"},
		{"", ""},
		{"Si", "Si"},
		{"SiZ66", "SiZ66"},
		{"BRZ6", "BRZ6"},
	} {
		if got := normalizeAlorSecID(tc[0], now); got != tc[1] {
			t.Fatalf("normalize(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestThrottleSpacing pins the client-side rate cap: back-to-back calls
// serialize ~100ms apart instead of fanning out.
func TestThrottleSpacing(t *testing.T) {
	m := &MarketClient{}
	start := time.Now()
	m.throttle()
	m.throttle()
	if el := time.Since(start); el < 90*time.Millisecond {
		t.Fatalf("calls not spaced: %v", el)
	}
}
