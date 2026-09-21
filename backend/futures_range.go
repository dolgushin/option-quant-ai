package main

// Futures daily-range tracker («Диапазоны» tab): how many points Si / ED / RI
// travel each day from the market open, plus the travelled share of ATR(14).
//
// Data comes from the public MOEX ISS candles (daily O/H/L/C) with a live
// today-row from the ISS marketdata OPEN/HIGH/LOW/LAST. Daily rows are also
// appended to DATA_DIR/range_daily.json so the stat accumulates across
// restarts even if ISS history windows shift.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rangeOHLC is one daily futures candle from MOEX ISS.
type rangeOHLC struct {
	Date        time.Time
	Open        float64
	High        float64
	Low         float64
	Close       float64
	Volume      float64
	VolumeKnown bool
}

// rangeDay is one calendar trading day with its travelled range and ATR.
type rangeDay struct {
	Date        string  `json:"date"` // YYYY-MM-DD
	Symbol      string  `json:"symbol"`
	SecID       string  `json:"secid"`
	Open        float64 `json:"open"`
	High        float64 `json:"high"`
	Low         float64 `json:"low"`
	Close       float64 `json:"close"`
	Range       float64 `json:"range"`         // high - low, points
	Change      float64 `json:"change"`        // close - open, points
	ATR14       float64 `json:"atr14"`         // Wilder ATR(14) as of this day, points
	RangePctATR float64 `json:"range_pct_atr"` // range / ATR * 100, %
	Volume      float64 `json:"volume"`
}

// rangeWeek aggregates one Mon–Fri trading week.
type rangeWeek struct {
	WeekStart string  `json:"week_start"` // Monday YYYY-MM-DD
	WeekEnd   string  `json:"week_end"`   // last trading day of the week
	Days      int     `json:"days"`
	SumRange  float64 `json:"sum_range"` // Σ daily ranges, points
	WeekHigh  float64 `json:"week_high"`
	WeekLow   float64 `json:"week_low"`
	WeekRange float64 `json:"week_range"` // weekHigh - weekLow, points
	AvgRange  float64 `json:"avg_range"`
	ATR       float64 `json:"atr"`         // Friday (last day) ATR14
	SumPctATR float64 `json:"sum_pct_atr"` // Σ daily range/ATR %
}

const rangeATRPeriod = 14

// rangeRound keeps Si/RI-style quotes at whole points and small-quoted
// underlyings (ED ~1.16) at 4 decimals.
func rangeRound(symbol string, v float64) float64 {
	if symbol == "ED" {
		return math.Round(v*10000) / 10000
	}
	return math.Round(v)
}

// parseRangeNumber decodes one ISS cell: JSON numbers arrive as float64,
// but quoted underlyings occasionally arrive as strings ("1,1608").
func parseRangeNumber(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		s := strings.ReplaceAll(strings.TrimSpace(n), ",", ".")
		s = strings.ReplaceAll(s, " ", "")
		f, _ := strconv.ParseFloat(s, 64)
		return f
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

// fetchRangeCandles pulls daily O/H/L/C candles for a FORTS secid.
func fetchRangeCandles(secid, from, till string) ([]rangeOHLC, error) {
	url := fmt.Sprintf("http://iss.moex.com/iss/engines/futures/markets/forts/boards/RFUD/securities/%s/candles.json?iss.meta=off&from=%s&till=%s&interval=24&candles.columns=begin,open,high,low,close,volume",
		secid, from, till)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("iss candles status %d for %s", resp.StatusCode, secid)
	}
	var data struct {
		Candles struct {
			Data [][]interface{} `json:"data"`
		} `json:"candles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	out := make([]rangeOHLC, 0, len(data.Candles.Data))
	for _, row := range data.Candles.Data {
		if len(row) < 5 {
			continue
		}
		begin, _ := row[0].(string)
		t, err := time.Parse("2006-01-02 15:04:05", begin)
		if err != nil {
			t, err = time.Parse("2006-01-02", begin)
			if err != nil {
				continue
			}
		}
		c := rangeOHLC{
			Date:  t,
			Open:  parseRangeNumber(row[1]),
			High:  parseRangeNumber(row[2]),
			Low:   parseRangeNumber(row[3]),
			Close: parseRangeNumber(row[4]),
		}
		if len(row) >= 6 {
			if v := parseRangeNumber(row[5]); v > 0 {
				c.Volume, c.VolumeKnown = v, true
			}
		}
		if c.Close <= 0 || c.High <= 0 || c.Low <= 0 {
			continue
		}
		if c.Open <= 0 {
			c.Open = c.Close
		}
		out = append(out, c)
	}
	return out, nil
}

// rangeTrueRange is the Wilder True Range of one day.
func rangeTrueRange(cur rangeOHLC, prevClose float64, first bool) float64 {
	if first {
		return cur.High - cur.Low
	}
	return math.Max(cur.High-cur.Low, math.Max(math.Abs(cur.High-prevClose), math.Abs(cur.Low-prevClose)))
}

// computeRangeATR returns the Wilder ATR(period) per candle index (0 while
// warming up). Pure — covered by unit tests.
func computeRangeATR(candles []rangeOHLC, period int) []float64 {
	atr := make([]float64, len(candles))
	if period <= 0 || len(candles) < period {
		return atr
	}
	tr := make([]float64, len(candles))
	for i, c := range candles {
		if i == 0 {
			tr[i] = rangeTrueRange(c, 0, true)
		} else {
			tr[i] = rangeTrueRange(c, candles[i-1].Close, false)
		}
	}
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += tr[i]
	}
	atr[period-1] = sum / float64(period)
	for i := period; i < len(candles); i++ {
		atr[i] = (atr[i-1]*float64(period-1) + tr[i]) / float64(period)
	}
	return atr
}

// buildRangeDays converts candles + ATR series into API rows. Pure.
func buildRangeDays(symbol, secid string, candles []rangeOHLC, atr []float64) []rangeDay {
	out := make([]rangeDay, 0, len(candles))
	for i, c := range candles {
		var a float64
		if i < len(atr) {
			a = atr[i]
		}
		rng := c.High - c.Low
		var pct float64
		if a > 0 {
			pct = rng / a * 100
		}
		out = append(out, rangeDay{
			Date:        c.Date.Format("2006-01-02"),
			Symbol:      symbol,
			SecID:       secid,
			Open:        rangeRound(symbol, c.Open),
			High:        rangeRound(symbol, c.High),
			Low:         rangeRound(symbol, c.Low),
			Close:       rangeRound(symbol, c.Close),
			Range:       rangeRound(symbol, rng),
			Change:      rangeRound(symbol, c.Close-c.Open),
			ATR14:       rangeRound(symbol, a),
			RangePctATR: math.Round(pct*10) / 10,
			Volume:      c.Volume,
		})
	}
	return out
}

// rangeWeekMonday maps a date to its week's Monday. Pure.
func rangeWeekMonday(t time.Time) time.Time {
	y, m, d := t.Date()
	t = time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	wd := int(t.Weekday()) // Sun=0 … Sat=6
	shift := (wd + 6) % 7  // Mon=0 … Sun=6
	return t.AddDate(0, 0, -shift)
}

// groupRangeWeeks folds daily rows into Mon–Fri week aggregates. Pure.
func groupRangeWeeks(days []rangeDay) []rangeWeek {
	groups := map[string][]rangeDay{}
	order := []string{}
	for _, d := range days {
		t, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			continue
		}
		monday := rangeWeekMonday(t).Format("2006-01-02")
		if _, ok := groups[monday]; !ok {
			order = append(order, monday)
		}
		groups[monday] = append(groups[monday], d)
	}
	// order is already chronological when input is chronological; sort anyway.
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			if order[j] < order[i] {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	out := make([]rangeWeek, 0, len(order))
	for _, mon := range order {
		rows := groups[mon]
		w := rangeWeek{WeekStart: mon, Days: len(rows)}
		var sum, sumPct float64
		hi, lo := -1e18, 1e18
		for _, r := range rows {
			sum += r.Range
			sumPct += r.RangePctATR
			if r.High > hi {
				hi, w.WeekHigh = r.High, r.High
			}
			if r.Low < lo {
				lo, w.WeekLow = r.Low, r.Low
			}
			w.WeekEnd = r.Date
			w.ATR = r.ATR14
		}
		_ = hi
		_ = lo
		w.SumRange = math.Round(sum*10000) / 10000
		w.SumPctATR = math.Round(sumPct*10) / 10
		w.WeekRange = math.Round((w.WeekHigh-w.WeekLow)*10000) / 10000
		if w.Days > 0 {
			w.AvgRange = math.Round(sum/float64(w.Days)*10000) / 10000
		}
		// Normalize integer-quoted symbols to whole points.
		if len(rows) > 0 && rows[0].Symbol != "ED" {
			w.SumRange = math.Round(sum)
			w.WeekRange = math.Round(w.WeekHigh - w.WeekLow)
			w.AvgRange = math.Round(sum / float64(w.Days))
		}
		out = append(out, w)
	}
	return out
}

// resolveRangeCode picks the tradable front contract for a range symbol:
// the selected series while it is still live, else the nearest live
// contract (the saved selection can point at an expired series — e.g. SiU6
// after the September expiry — whose board is dead). ED/RI/Si all resolve
// through the same futures board — no trade-universe changes needed.
func resolveRangeCode(symbol string) string {
	today := time.Now().Format("2006-01-02")
	if code := selectedSeriesFor(symbol); code != "" {
		if isSyntheticSeriesCode(code) {
			if real := resolveRealFuturesCode(symbol, code); real != "" {
				return real
			}
			return code
		}
		if c := findContractByCode(code); c != nil {
			if c.LastDelDate >= today {
				return code
			}
			// Selected contract expired — fall through to nearest live.
		} else if nearest := resolveNearestFuturesCode(symbol); nearest != "" {
			// Unknown code (expired and delisted) — switch to live contract.
			return nearest
		} else {
			return code // ISS hiccup — keep old code, errors surface downstream.
		}
	}
	return resolveNearestFuturesCode(symbol)
}

// fetchRangeHistory loads daily rows with ATR for a symbol over [from, till].
func fetchRangeHistory(symbol, from, till string) ([]rangeDay, string, error) {
	secid := resolveRangeCode(symbol)
	if secid == "" {
		return nil, "", fmt.Errorf("нет фьючерсной серии для %s", symbol)
	}
	candles, err := fetchRangeCandles(secid, from, till)
	if err != nil {
		return nil, secid, err
	}
	atr := computeRangeATR(candles, rangeATRPeriod)
	days := buildRangeDays(symbol, secid, candles, atr)
	recordRangeDays(symbol, days)
	return days, secid, nil
}

// ---- persistent daily store (DATA_DIR/range_daily.json) ----

var (
	rangeStoreMu sync.Mutex
	rangeStore   = map[string]map[string]rangeDay{}
	rangeLoaded  = false
)

func rangeStoreFile() string {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	return filepath.Join(dir, "range_daily.json")
}

func loadRangeStore() {
	b, err := os.ReadFile(rangeStoreFile())
	if err != nil {
		return
	}
	var m map[string]map[string]rangeDay
	if err := json.Unmarshal(b, &m); err != nil {
		return
	}
	rangeStore = m
}

// recordRangeDays merges daily rows into the on-disk accumulator (best effort).
func recordRangeDays(symbol string, days []rangeDay) {
	if len(days) == 0 {
		return
	}
	rangeStoreMu.Lock()
	defer rangeStoreMu.Unlock()
	if !rangeLoaded {
		loadRangeStore()
		rangeLoaded = true
	}
	if rangeStore == nil {
		rangeStore = map[string]map[string]rangeDay{}
	}
	byDate := rangeStore[symbol]
	if byDate == nil {
		byDate = map[string]rangeDay{}
		rangeStore[symbol] = byDate
	}
	for _, d := range days {
		byDate[d.Date] = d
	}
	b, err := json.Marshal(rangeStore)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(rangeStoreFile()), 0o755)
	_ = os.WriteFile(rangeStoreFile(), b, 0o644)
}

// ---- caches ----

type rangeHistEntry struct {
	at   time.Time
	days []rangeDay
	sec  string
}

var (
	rangeHistMu    sync.Mutex
	rangeHistCache = map[string]rangeHistEntry{}
)

func cachedRangeHistory(symbol, from, till string) ([]rangeDay, string, error) {
	key := symbol + "|" + from + "|" + till
	rangeHistMu.Lock()
	if e, ok := rangeHistCache[key]; ok && time.Since(e.at) < 10*time.Minute {
		days := e.days
		sec := e.sec
		rangeHistMu.Unlock()
		return days, sec, nil
	}
	rangeHistMu.Unlock()
	days, sec, err := fetchRangeHistory(symbol, from, till)
	if err != nil {
		return nil, sec, err
	}
	rangeHistMu.Lock()
	rangeHistCache[key] = rangeHistEntry{at: time.Now(), days: days, sec: sec}
	rangeHistMu.Unlock()
	return days, sec, nil
}

// rangeTodayQuote is the live marketdata snapshot for one symbol.
func rangeTodayQuote(secid string) (open, high, low, last, vol float64, err error) {
	url := fmt.Sprintf("http://iss.moex.com/iss/engines/futures/markets/forts/boards/RFUD/securities/%s.json?iss.meta=off&iss.only=marketdata&marketdata.columns=SECID,OPEN,HIGH,LOW,LAST,VOLTODAY", secid)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, 0, 0, 0, fmt.Errorf("iss marketdata status %d", resp.StatusCode)
	}
	var data struct {
		Marketdata struct {
			Data [][]interface{} `json:"data"`
		} `json:"marketdata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, 0, 0, 0, 0, err
	}
	if len(data.Marketdata.Data) == 0 || len(data.Marketdata.Data[0]) < 5 {
		return 0, 0, 0, 0, 0, fmt.Errorf("no marketdata for %s", secid)
	}
	row := data.Marketdata.Data[0]
	open = parseRangeNumber(row[1])
	high = parseRangeNumber(row[2])
	low = parseRangeNumber(row[3])
	last = parseRangeNumber(row[4])
	if len(row) >= 6 {
		vol = parseRangeNumber(row[5])
	}
	return open, high, low, last, vol, nil
}

type rangeToday struct {
	Symbol      string  `json:"symbol"`
	SecID       string  `json:"secid"`
	Date        string  `json:"date"`
	Open        float64 `json:"open"`
	High        float64 `json:"high"`
	Low         float64 `json:"low"`
	Last        float64 `json:"last"`
	Range       float64 `json:"range"`
	Change      float64 `json:"change"`
	ATR14       float64 `json:"atr14"`
	RangePctATR float64 `json:"range_pct_atr"`
	Volume      float64 `json:"volume"`
	Stale       bool    `json:"stale,omitempty"`
}

var (
	rangeTodayMu    sync.Mutex
	rangeTodayCache = map[string]struct {
		at time.Time
		v  rangeToday
	}{}
)

// fetchRangeToday builds the live "from market open" row for a symbol.
func fetchRangeToday(symbol string) (rangeToday, error) {
	rangeTodayMu.Lock()
	if e, ok := rangeTodayCache[symbol]; ok && time.Since(e.at) < 15*time.Second {
		v := e.v
		rangeTodayMu.Unlock()
		return v, nil
	}
	rangeTodayMu.Unlock()

	secid := resolveRangeCode(symbol)
	if secid == "" {
		return rangeToday{}, fmt.Errorf("нет фьючерсной серии для %s", symbol)
	}
	open, high, low, last, vol, err := rangeTodayQuote(secid)
	if err != nil {
		return rangeToday{}, err
	}
	// ATR14 from the trailing daily history (warmup window + today excluded).
	till := time.Now().Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -60).Format("2006-01-02")
	var atr float64
	if candles, cerr := fetchRangeCandles(secid, from, till); cerr == nil && len(candles) >= rangeATRPeriod {
		series := computeRangeATR(candles, rangeATRPeriod)
		for i := len(series) - 1; i >= 0; i-- {
			if series[i] > 0 {
				atr = series[i]
				break
			}
		}
	}
	rng := 0.0
	if high > 0 && low > 0 {
		rng = high - low
	}
	chg := 0.0
	if last > 0 && open > 0 {
		chg = last - open
	}
	var pct float64
	if atr > 0 {
		pct = rng / atr * 100
	}
	// Weekend/holiday: today's marketdata row is the previous session's
	// carry-over (OPEN==0 or all-equal). Flag it so the UI can say "торги закрыты".
	stale := open <= 0
	out := rangeToday{
		Symbol: symbol, SecID: secid, Date: time.Now().Format("2006-01-02"),
		Open: rangeRound(symbol, open), High: rangeRound(symbol, high),
		Low: rangeRound(symbol, low), Last: rangeRound(symbol, last),
		Range: rangeRound(symbol, rng), Change: rangeRound(symbol, chg),
		ATR14: rangeRound(symbol, atr), RangePctATR: math.Round(pct*10) / 10,
		Volume: vol, Stale: stale,
	}
	// Persist the live snapshot as today's row (close = last tick).
	recordRangeDays(symbol, []rangeDay{{
		Date: out.Date, Symbol: symbol, SecID: secid,
		Open: out.Open, High: out.High, Low: out.Low, Close: out.Last,
		Range: out.Range, Change: out.Change, ATR14: out.ATR14,
		RangePctATR: out.RangePctATR, Volume: vol,
	}})
	rangeTodayMu.Lock()
	rangeTodayCache[symbol] = struct {
		at time.Time
		v  rangeToday
	}{at: time.Now(), v: out}
	rangeTodayMu.Unlock()
	return out, nil
}

// normalizeRangeSymbol maps user input to the MOEX root spelling ("Si" is
// mixed-case on the board; strings.ToUpper would break prefix matching).
func normalizeRangeSymbol(s string) string {
	t := strings.TrimSpace(s)
	switch {
	case strings.EqualFold(t, "si"):
		return "Si"
	case strings.EqualFold(t, "ri"):
		return "RI"
	case strings.EqualFold(t, "ed"):
		return "ED"
	case strings.EqualFold(t, "cr"):
		return "CR"
	default:
		return t
	}
}

func parseRangeSymbols(r *http.Request) []string {
	raw := r.URL.Query().Get("symbols")
	if raw == "" {
		raw = r.URL.Query().Get("symbol")
	}
	if raw == "" {
		return []string{"Si", "ED"}
	}
	parts := strings.Split(raw, ",")
	out := []string{}
	seen := map[string]bool{}
	for _, p := range parts {
		s := normalizeRangeSymbol(p)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= 5 {
			break
		}
	}
	if len(out) == 0 {
		return []string{"Si", "ED"}
	}
	return out
}

// rangeTodayHandler returns the live "from market open" rows.
// URL: /api/v1/range/today?symbols=Si,ED
func rangeTodayHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbols := parseRangeSymbols(r)
	type rowOut struct {
		rangeToday
		Err string `json:"err,omitempty"`
	}
	rows := make([]rowOut, 0, len(symbols))
	for _, s := range symbols {
		t, err := fetchRangeToday(s)
		ro := rowOut{rangeToday: t}
		if err != nil {
			ro.Err = err.Error()
		}
		rows = append(rows, ro)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"as_of":   time.Now().Format(time.RFC3339),
		"symbols": rows,
	})
}

// rangeHistoryHandler returns daily rows with ATR.
// URL: /api/v1/range/history?symbol=Si&days=30  (or &from=2026-09-01&till=2026-09-21)
func rangeHistoryHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := normalizeRangeSymbol(r.URL.Query().Get("symbol"))
	if symbol == "" {
		symbol = "Si"
	}
	from := r.URL.Query().Get("from")
	till := r.URL.Query().Get("till")
	if from == "" || till == "" {
		days := 30
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 2 && n <= 180 {
				days = n
			}
		}
		till = time.Now().Format("2006-01-02")
		// +20 days warmup so the first visible rows already carry ATR.
		from = time.Now().AddDate(0, 0, -(days + 20)).Format("2006-01-02")
		rows, secid, err := cachedRangeHistory(symbol, from, till)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
			return
		}
		if len(rows) > days {
			rows = rows[len(rows)-days:]
		}
		var lastATR float64
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].ATR14 > 0 {
				lastATR = rows[i].ATR14
				break
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"symbol": symbol, "secid": secid, "atr14": lastATR, "days": rows,
		})
		return
	}
	rows, secid, err := cachedRangeHistory(symbol, from, till)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol": symbol, "secid": secid, "days": rows,
	})
}

// rangeWeeksHandler aggregates daily rows into Mon–Fri weeks.
// URL: /api/v1/range/weeks?symbol=Si&weeks=8
func rangeWeeksHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := normalizeRangeSymbol(r.URL.Query().Get("symbol"))
	if symbol == "" {
		symbol = "Si"
	}
	weeks := 8
	if v := r.URL.Query().Get("weeks"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 26 {
			weeks = n
		}
	}
	till := time.Now().Format("2006-01-02")
	from := time.Now().AddDate(0, 0, -(weeks*7 + 30)).Format("2006-01-02")
	rows, secid, err := cachedRangeHistory(symbol, from, till)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	all := groupRangeWeeks(rows)
	if len(all) > weeks {
		all = all[len(all)-weeks:]
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol": symbol, "secid": secid, "weeks": all,
	})
}

// ---- intraday ranges (5 / 10 / 60 minutes) ----

// rangeBar is one intraday candle with its travelled range.
type rangeBar struct {
	Time   string  `json:"time"` // YYYY-MM-DD HH:MM
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Range  float64 `json:"range"` // high - low, points
	Volume float64 `json:"volume"`
}

// parseRangeTF whitelists the intraday timeframe minutes.
func parseRangeTF(s string) int {
	switch s {
	case "5", "10", "60":
		n, _ := strconv.Atoi(s)
		return n
	default:
		return 5
	}
}

// fetchIntradayCandles pulls intraday O/H/L/C bars for a FORTS secid.
// ISS interval is in minutes; rows come back oldest-first. ISS pages at 500
// rows, so we follow `start` offsets until a short page (cap 2500 rows).
// intervals 5/15/30 come back empty on the futures board, so tf=5 is served
// by aggregating 1-minute candles (aggregateBars).
func fetchIntradayCandles(secid string, tf int, from, till string) ([]rangeOHLC, error) {
	interval := tf
	if tf == 5 {
		interval = 1
	}
	client := &http.Client{Timeout: 15 * time.Second}
	out := []rangeOHLC{}
	for start := 0; start < 2500; start += 500 {
		url := fmt.Sprintf("http://iss.moex.com/iss/engines/futures/markets/forts/boards/RFUD/securities/%s/candles.json?iss.meta=off&from=%s&till=%s&interval=%d&start=%d&candles.columns=begin,open,high,low,close,volume",
			secid, from, till, interval, start)
		resp, err := client.Get(url)
		if err != nil {
			return nil, err
		}
		var data struct {
			Candles struct {
				Data [][]interface{} `json:"data"`
			} `json:"candles"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if derr != nil {
			return nil, derr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("iss intraday status %d for %s", resp.StatusCode, secid)
		}
		n := 0
		for _, row := range data.Candles.Data {
			if len(row) < 5 {
				continue
			}
			begin, _ := row[0].(string)
			t, err := time.Parse("2006-01-02 15:04:05", begin)
			if err != nil {
				continue
			}
			c := rangeOHLC{
				Date:  t,
				Open:  parseRangeNumber(row[1]),
				High:  parseRangeNumber(row[2]),
				Low:   parseRangeNumber(row[3]),
				Close: parseRangeNumber(row[4]),
			}
			if len(row) >= 6 {
				if v := parseRangeNumber(row[5]); v > 0 {
					c.Volume, c.VolumeKnown = v, true
				}
			}
			if c.Close <= 0 || c.High <= 0 || c.Low <= 0 {
				continue
			}
			if c.Open <= 0 {
				c.Open = c.Close
			}
			out = append(out, c)
			n++
		}
		if n < 500 {
			break
		}
	}
	if tf == 5 {
		out = aggregateBars(out, 5)
	}
	return out, nil
}

// aggregateBars folds 1-minute candles into n-minute buckets (bucket start =
// minute floored to n). Pure — covered by unit tests.
func aggregateBars(candles []rangeOHLC, n int) []rangeOHLC {
	out := []rangeOHLC{}
	var cur *rangeOHLC
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, c := range candles {
		// Floor wall-clock minutes so bars align to :00/:05/:10…
		// (time.Truncate would align to UTC midnight instead).
		y, mo, dd := c.Date.Date()
		hh, mm, _ := c.Date.Clock()
		bucket := time.Date(y, mo, dd, hh, mm/n*n, 0, 0, c.Date.Location())
		if cur == nil || !cur.Date.Equal(bucket) {
			flush()
			cp := c
			cp.Date = bucket
			cur = &cp
			continue
		}
		if c.High > cur.High {
			cur.High = c.High
		}
		if c.Low < cur.Low {
			cur.Low = c.Low
		}
		cur.Close = c.Close
		cur.Volume += c.Volume
		cur.VolumeKnown = cur.VolumeKnown || c.VolumeKnown
	}
	flush()
	return out
}

// buildIntradayBars converts raw bars into API rows with per-bar ranges.
// Pure — covered by unit tests.
func buildIntradayBars(symbol string, candles []rangeOHLC) []rangeBar {
	out := make([]rangeBar, 0, len(candles))
	for _, c := range candles {
		out = append(out, rangeBar{
			Time:   c.Date.Format("2006-01-02 15:04"),
			Open:   rangeRound(symbol, c.Open),
			High:   rangeRound(symbol, c.High),
			Low:    rangeRound(symbol, c.Low),
			Close:  rangeRound(symbol, c.Close),
			Range:  rangeRound(symbol, c.High-c.Low),
			Volume: c.Volume,
		})
	}
	return out
}

// intradayAvgRange is the mean bar range over the window. Pure.
func intradayAvgRange(bars []rangeBar) float64 {
	if len(bars) == 0 {
		return 0
	}
	sum := 0.0
	for _, b := range bars {
		sum += b.Range
	}
	return sum / float64(len(bars))
}

type rangeIntraEntry struct {
	at   time.Time
	bars []rangeBar
	sec  string
}

var (
	rangeIntraMu    sync.Mutex
	rangeIntraCache = map[string]rangeIntraEntry{}
)

// rangeIntradayHandler returns per-bar travelled ranges for today (+previous
// session for context), capped to the freshest bars.
// URL: /api/v1/range/intraday?symbol=Si&tf=5|10|60
func rangeIntradayHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := normalizeRangeSymbol(r.URL.Query().Get("symbol"))
	if symbol == "" {
		symbol = "Si"
	}
	tf := parseRangeTF(r.URL.Query().Get("tf"))
	key := fmt.Sprintf("%s|%d", symbol, tf)

	rangeIntraMu.Lock()
	if e, ok := rangeIntraCache[key]; ok && time.Since(e.at) < time.Minute {
		bars, sec := e.bars, e.sec
		rangeIntraMu.Unlock()
		json.NewEncoder(w).Encode(rangeIntradayPayload(symbol, sec, tf, bars))
		return
	}
	rangeIntraMu.Unlock()

	secid := resolveRangeCode(symbol)
	if secid == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": fmt.Sprintf("нет фьючерсной серии для %s", symbol)})
		return
	}
	// Two trading days cover today plus context; till is tomorrow so the
	// still-forming evening bar is included.
	from := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
	till := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	candles, err := fetchIntradayCandles(secid, tf, from, till)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	bars := buildIntradayBars(symbol, candles)
	// FORTS day is long (07:00–23:50 MSK): keep the freshest ~400 bars so
	// the chart stays readable (≈2 sessions on 5m, ≈a week on 60m).
	const maxBars = 400
	if len(bars) > maxBars {
		bars = bars[len(bars)-maxBars:]
	}
	rangeIntraMu.Lock()
	rangeIntraCache[key] = rangeIntraEntry{at: time.Now(), bars: bars, sec: secid}
	rangeIntraMu.Unlock()
	json.NewEncoder(w).Encode(rangeIntradayPayload(symbol, secid, tf, bars))
}

// rangeIntradayPayload shapes the intraday response incl. the mean bar range
// (the reference line on the chart) and the max bar.
func rangeIntradayPayload(symbol, secid string, tf int, bars []rangeBar) map[string]interface{} {
	avg := intradayAvgRange(bars)
	maxR := 0.0
	for _, b := range bars {
		if b.Range > maxR {
			maxR = b.Range
		}
	}
	if symbol == "ED" {
		avg = math.Round(avg*10000) / 10000
		maxR = math.Round(maxR*10000) / 10000
	} else {
		avg = math.Round(avg)
		maxR = math.Round(maxR)
	}
	if bars == nil {
		bars = []rangeBar{}
	}
	return map[string]interface{}{
		"symbol": symbol, "secid": secid, "tf": tf,
		"avg_range": avg, "max_range": maxR, "count": len(bars), "bars": bars,
	}
}
