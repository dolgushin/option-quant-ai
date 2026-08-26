package main

// Calendar module: corporate events (dividends, earnings) for MOEX instruments.
// Used by the spread builder and candidate scoring to flag risky expiries.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

type calendarEvent struct {
	Date    string `json:"date"`    // YYYY-MM-DD
	Symbol  string `json:"symbol"`  // SBER, SBERP, etc.
	Type    string `json:"type"`    // "dividend" or "earnings"
	Detail  string `json:"detail"`  // e.g. "₽25.00/акция"
}

// eventCalendar is a static (hardcoded) calendar of known upcoming events.
// Extend this as new dates are announced by MOEX issuers.
var eventCalendar = []calendarEvent{
	// SBER dividends 2026 (examples — update when official dates are published)
	{Date: "2026-07-11", Symbol: "SBER", Type: "dividend", Detail: "Ожидается ~₽25/акция (mai-2026)"},
	{Date: "2026-12-11", Symbol: "SBER", Type: "dividend", Detail: "Ожидается (dec-2026)"},
	// SBERP dividends 2026
	{Date: "2026-07-11", Symbol: "SBERP", Type: "dividend", Detail: "Ожидается ~₽50/акция (mai-2026)"},
	{Date: "2026-12-11", Symbol: "SBERP", Type: "dividend", Detail: "Ожидается (dec-2026)"},
}

// calendarHandler returns upcoming events, optionally filtered by symbol.
// GET /api/v1/calendar?symbol=SBER&days=90
func calendarHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	days := 90
	if d := r.URL.Query().Get("days"); d != "" {
		var v int
		if _, err := fmt.Sscanf(d, "%d", &v); err == nil && v > 0 && v <= 365 {
			days = v
		}
	}

	now := time.Now()
	deadline := now.AddDate(0, 0, days)
	var result []calendarEvent
	for _, e := range eventCalendar {
		t, err := time.Parse("2006-01-02", e.Date)
		if err != nil || t.Before(now) || t.After(deadline) {
			continue
		}
		if symbol != "" && e.Symbol != symbol {
			continue
		}
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Date < result[j].Date })
	json.NewEncoder(w).Encode(map[string]interface{}{
		"events": result,
		"range":  map[string]string{"from": now.Format("2006-01-02"), "to": deadline.Format("2006-01-02")},
	})
}

// calendarWarning returns true if the given expiry date falls within ±3 days
// of a known event for the symbol. Used by spread scoring to penalise risky expiries.
func calendarWarning(symbol, expiry string) (bool, string) {
	exp, err := time.Parse("2006-01-02", expiry)
	if err != nil {
		return false, ""
	}
	for _, e := range eventCalendar {
		if e.Symbol != symbol {
			continue
		}
		evt, err := time.Parse("2006-01-02", e.Date)
		if err != nil {
			continue
		}
		diff := exp.Sub(evt).Hours() / 24
		if diff >= -3 && diff <= 3 {
			return true, e.Detail
		}
	}
	return false, ""
}
