package main

import (
	"testing"

	"option-quant-ai/alor"
)

// TestAlorEntryStrikeKind pins directory entry parsing: strikes from digit
// runs (first ≥ 1000), kind by explicit CALL/PUT words, futures and stocks
// skipped (no strike, no kind).
func TestAlorEntryStrikeKind(t *testing.T) {
	mk := func(sym, short, desc string) alor.AlorSecurityResponse {
		return alor.AlorSecurityResponse{Symbol: sym, ShortName: short, Description: desc}
	}
	cases := []struct {
		e      alor.AlorSecurityResponse
		strike float64
		kind   string
	}{
		{mk("Si86600BO6A", "Si 86000 Call", ""), 86000, "call"},
		{mk("Si86600BO6B", "Si 86000 Put", ""), 86000, "put"},
		{mk("X", "Si-12.26", "futures"), 0, ""},
		{mk("SIBN", "SIBN", ""), 0, ""},
		{mk("", "", ""), 0, ""},
	}
	for i, c := range cases {
		if got := alorEntryStrike(c.e); got != c.strike {
			t.Fatalf("case %d strike = %v, want %v", i, got, c.strike)
		}
		if got := alorEntryKind(c.e); got != c.kind {
			t.Fatalf("case %d kind = %q, want %q", i, got, c.kind)
		}
	}
}

// TestNormalizeAlorDate pins expiry matching across layouts.
func TestNormalizeAlorDate(t *testing.T) {
	for in, want := range map[string]string{
		"2026-12-17":           "2026-12-17",
		"17.12.2026":           "2026-12-17",
		"2026-12-17T19:00:00Z": "2026-12-17",
		"20261217":             "2026-12-17",
		"garbage":              "",
		"":                     "",
	} {
		if got := normalizeAlorDate(in); got != want {
			t.Fatalf("date %q = %q, want %q", in, got, want)
		}
	}
}

// stubDetail serves canned (strike, expiry, kind) per secid for the matcher.
func stubDetail(m map[string][3]string) func(string) (float64, string, string) {
	return func(id string) (float64, string, string) {
		v, ok := m[id]
		if !ok {
			return 0, "", ""
		}
		return 86000, v[1], v[2]
	}
}

// TestResolveOptionEntry pins deterministic directory matching: exact
// (strike, kind, expiry) wins, expiry mismatches refuse, kind mismatches
// refuse, ambiguity across series refuses instead of guessing.
func TestResolveOptionEntry(t *testing.T) {
	mk := func(sym, short string) alor.AlorSecurityResponse {
		return alor.AlorSecurityResponse{Symbol: sym, ShortName: short}
	}
	// Single confirmed series resolves.
	got, err := resolveOptionEntry(
		[]alor.AlorSecurityResponse{mk("A1", "Si 86000 Call"), mk("A2", "Si 86000 Put")},
		86000, true, "2026-12-17",
		stubDetail(map[string][3]string{"A1": {"", "2026-12-17", "call"}, "A2": {"", "2026-12-17", "put"}}),
	)
	if err != nil || got != "A1" {
		t.Fatalf("single = %q, %v; want A1, nil", got, err)
	}
	// Same strike+kind in two series: only the dated one wins.
	got, err = resolveOptionEntry(
		[]alor.AlorSecurityResponse{mk("A1", "Si 86000 Call"), mk("A3", "Si 86000 Call")},
		86000, true, "2026-10-17",
		stubDetail(map[string][3]string{"A1": {"", "2026-12-17", "call"}, "A3": {"", "17.10.2026", "call"}}),
	)
	if err != nil || got != "A3" {
		t.Fatalf("dated = %q, %v; want A3, nil", got, err)
	}
	// No dated match refuses.
	if _, err := resolveOptionEntry(
		[]alor.AlorSecurityResponse{mk("A1", "Si 86000 Call")},
		86000, true, "2027-01-01",
		stubDetail(map[string][3]string{"A1": {"", "2026-12-17", "call"}}),
	); err == nil {
		t.Fatal("dateless must refuse")
	}
	// Puts never match a call request.
	if _, err := resolveOptionEntry(
		[]alor.AlorSecurityResponse{mk("A2", "Si 86000 Put")},
		86000, true, "2026-12-17",
		stubDetail(map[string][3]string{"A2": {"", "2026-12-17", "put"}}),
	); err == nil {
		t.Fatal("kind mismatch must refuse")
	}
	// Futures and stocks never become candidates.
	if _, err := resolveOptionEntry(
		[]alor.AlorSecurityResponse{{Symbol: "X", ShortName: "Si-12.26"}, {Symbol: "SIBN"}},
		86000, true, "2026-12-17",
		stubDetail(map[string][3]string{}),
	); err == nil {
		t.Fatal("non-options must refuse")
	}
}
