package main

// Alor-native option resolution.
//
// MOEX-format secids are never quoted blindly on Alor (499 toward the ban):
// every option quote resolves through the Alor securities directory by
// (strike, call/put, expiry). Direct secid quotes are tried first (zero
// extra traffic when Alor accepts the secid as-is); directory remap second;
// unresolvable legs refuse loudly instead of trading on guesses.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"option-quant-ai/alor"
)

var (
	alorDirMu    sync.Mutex
	alorDirCache = map[string]alorDirEntry{}
)

// alorSecidCache maps (symbol|strike|call|expiry) → Alor-native secid (1h
// TTL — series don't roll intraday). Spares repeat detail lookups on every
// book pull.
var (
	alorSecidMu    sync.Mutex
	alorSecidCache = map[string]alorSecidEntry{}
)

type alorSecidEntry struct {
	At    time.Time
	SecID string
}

func alorCachedResolve(symbol string, strike float64, isCall bool, expiry string) (string, error) {
	key := fmt.Sprintf("%s|%g|%v|%s", symbol, strike, isCall, expiry)
	alorSecidMu.Lock()
	e, ok := alorSecidCache[key]
	alorSecidMu.Unlock()
	if ok && time.Since(e.At) < time.Hour {
		return e.SecID, nil
	}
	code, err := alorResolveOptionSecID(symbol, strike, isCall, expiry)
	if err != nil {
		return "", err
	}
	alorSecidMu.Lock()
	alorSecidCache[key] = alorSecidEntry{At: time.Now(), SecID: code}
	alorSecidMu.Unlock()
	return code, nil
}

// alorBookForLeg fetches the order book of an option leg: the stored secid
// first (zero extra traffic when Alor accepts it), directory-remapped Alor
// secid second, loud refusal last. Never MOEX.
func alorBookForLeg(symbol string, strike float64, isCall bool, expiry, secid string) (alor.AlorOrderbookResponse, error) {
	var empty alor.AlorOrderbookResponse
	if alorMarket == nil {
		return empty, fmt.Errorf("alor не настроен")
	}
	if ob, err := alorMarket.FetchOrderbook("MOEX", secid); err == nil {
		return ob, nil
	}
	code, err := alorCachedResolve(symbol, strike, isCall, expiry)
	if err != nil {
		return empty, err
	}
	if ob, err := alorMarket.FetchOrderbook("MOEX", code); err == nil {
		log.Printf("alor book remap %s → %s", secid, code)
		return ob, nil
	} else {
		return empty, err
	}
}

// alorLegMidEx is alorLegMid via the remapping book path (mid or 0).
func alorLegMidEx(symbol string, strike float64, isCall bool, expiry, secid string) float64 {
	ob, err := alorBookForLeg(symbol, strike, isCall, expiry, secid)
	if err != nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return 0
	}
	bid, ask := ob.Bids[0].Price, ob.Asks[0].Price
	if bid <= 0 || ask < bid {
		return 0
	}
	return (bid + ask) / 2
}

// alorDirEntry is one cached securities directory (per root, 15 min TTL —
// series don't roll intraday).
type alorDirEntry struct {
	At      time.Time
	Entries []alor.AlorSecurityResponse
}

// alorOptionDirectory fetches the Alor securities directory for root.
func alorOptionDirectory(root string) ([]alor.AlorSecurityResponse, error) {
	if alorMarket == nil {
		return nil, fmt.Errorf("alor не настроен")
	}
	alorDirMu.Lock()
	e, ok := alorDirCache[root]
	alorDirMu.Unlock()
	if ok && time.Since(e.At) < 15*time.Minute {
		return e.Entries, nil
	}
	entries, err := alorMarket.FetchSecurities(root)
	if err != nil {
		return nil, err
	}
	alorDirMu.Lock()
	alorDirCache[root] = alorDirEntry{At: time.Now(), Entries: entries}
	alorDirMu.Unlock()
	return entries, nil
}

// entryNumbers splits text into number tokens.
func entryNumbers(text string) []float64 {
	out := []float64{}
	for _, f := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r == '.' || r == ',')
	}) {
		if v, err := strconv.ParseFloat(strings.ReplaceAll(f, ",", "."), 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// alorEntryStrike extracts the strike from shortname/symbol digits: the
// first number ≥ 1000 (dates and month fragments are smaller; futures like
// Si-12.26 and stocks yield 0 and are skipped as candidates).
func alorEntryStrike(e alor.AlorSecurityResponse) float64 {
	for _, text := range []string{e.ShortName, e.Symbol} {
		for _, v := range entryNumbers(text) {
			if v >= 1000 {
				return v
			}
		}
	}
	return 0
}

// alorEntryKind returns "call"/"put"/"" by explicit words (same rule as
// parseAlorOptionInfo — no positional code guessing).
func alorEntryKind(e alor.AlorSecurityResponse) string {
	u := strings.ToUpper(e.ShortName + " " + e.Description + " " + e.Symbol)
	hasC := strings.Contains(u, "CALL")
	hasP := strings.Contains(u, "PUT")
	if hasC && !hasP {
		return "call"
	}
	if hasP && !hasC {
		return "put"
	}
	return ""
}

// normalizeAlorDate folds an instrument date into YYYY-MM-DD, "" when
// unparseable (such entries never match an expiry — never guess series).
func normalizeAlorDate(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "T "); i > 0 {
		s = s[:i]
	}
	for _, layout := range []string{"2006-01-02", "02.01.2006", time.RFC3339, "2006-01-02T15:04:05", "20060102", "2006/01/02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// resolveOptionEntry picks the directory entry matching (strike, kind,
// expiry). detail fetches authoritative (strike, expiry, kind) per candidate
// secid (instrument details endpoint); candidates whose details don't
// confirm are skipped. Pure apart from detail — unit-tested with stubs.
func resolveOptionEntry(entries []alor.AlorSecurityResponse, strike float64, isCall bool, expiry string, detail func(secid string) (float64, string, string)) (string, error) {
	if strike <= 0 || expiry == "" {
		return "", fmt.Errorf("нужны страйк и экспирация")
	}
	want := "put"
	if isCall {
		want = "call"
	}
	cands := []string{}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Symbol == "" || seen[e.Symbol] {
			continue
		}
		if math.Abs(alorEntryStrike(e)-strike) > 0.51 {
			continue
		}
		if alorEntryKind(e) != want {
			continue
		}
		seen[e.Symbol] = true
		cands = append(cands, e.Symbol)
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("нет %.0f %s в каталоге Alor", strike, want)
	}
	for _, id := range cands {
		s, ex, k := detail(id)
		if math.Abs(s-strike) > 0.51 || k != want {
			continue
		}
		if normalizeAlorDate(ex) == expiry {
			return id, nil
		}
	}
	return "", fmt.Errorf("из %d кандидатов ни один не на %s", len(cands), expiry)
}

// alorInstrumentDetail fetches authoritative (strike, expiry, kind) for one
// Alor secid via the instrument details endpoint.
func alorInstrumentDetail(secid string) (float64, string, string) {
	if alorMarket == nil {
		return 0, "", ""
	}
	code, body, err := alorMarket.RawGet("/md/v2/Securities/MOEX/"+secid, "")
	if err != nil || code != 200 {
		return 0, "", ""
	}
	var raw map[string]interface{}
	if jerr := json.Unmarshal(body, &raw); jerr != nil {
		return 0, "", ""
	}
	return parseAlorOptionInfo(raw)
}

// alorResolveOptionSecID resolves the Alor-native secid for
// (symbol, strike, call/put, expiry YYYY-MM-DD).
func alorResolveOptionSecID(symbol string, strike float64, isCall bool, expiry string) (string, error) {
	entries, err := alorOptionDirectory(symbol)
	if err != nil {
		return "", err
	}
	return resolveOptionEntry(entries, strike, isCall, expiry, alorInstrumentDetail)
}

// alorQuoteOption quotes an option on Alor: direct secid first (zero extra
// traffic when Alor accepts it), directory remap second, loud refusal last.
// Returns the quote and how it was sourced ("direct"/"remapped").
func alorQuoteOption(symbol string, strike float64, isCall bool, expiry, moexSecID string) (optionQuoteEx, string, error) {
	if alorMarket == nil {
		return optionQuoteEx{}, "", fmt.Errorf("alor не настроен")
	}
	if q, err := alorOptionQuoteEx(moexSecID); err == nil && q.Price > 0 {
		return q, "direct", nil
	}
	code, err := alorResolveOptionSecID(symbol, strike, isCall, expiry)
	if err != nil {
		return optionQuoteEx{}, "", err
	}
	q, err := alorOptionQuoteEx(code)
	if err != nil || q.Price <= 0 {
		return optionQuoteEx{}, "", fmt.Errorf("нет котировки %s", code)
	}
	log.Printf("alor remap %s → %s", moexSecID, code)
	return q, "remapped", nil
}

// cachedAlorQuote is cachedAlorQuoteEx's tuple twin: quote cache keyed by the
// stored (MOEX) secid, filled via Alor-only resolution. Marks everywhere
// flow through here — no MOEX fallback inside.
func cachedAlorQuote(symbol string, strike float64, isCall bool, expiry, moexSecID string) (optionQuoteEx, bool) {
	quoteMu.Lock()
	if q, ok := quoteCache[moexSecID]; ok && q.Cached.Add(quoteTTL).After(time.Now()) {
		quoteMu.Unlock()
		return q, true
	}
	quoteMu.Unlock()
	q, _, err := alorQuoteOption(symbol, strike, isCall, expiry, moexSecID)
	if err != nil || q.Price <= 0 {
		return optionQuoteEx{}, false
	}
	q.Cached = time.Now()
	quoteMu.Lock()
	quoteCache[moexSecID] = q
	quoteMu.Unlock()
	return q, true
}
