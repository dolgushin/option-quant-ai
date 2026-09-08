package main

// Crypto options radar (BTC/ETH) in the style of the "OPCIONNY RADAR" card:
// price block, IV-by-expiry table, IV term structure, IV-Rank gauge,
// IV skew smile and an auto-generated Russian summary.
//
// Data sources (public, no keys):
//   - spot + daily history: Binance klines, fallback Bybit kline
//   - option IV: Deribit book summary by currency (single call, mark_iv)
//   - 1M IV history / week-ago reference: Deribit volatility-index history
//     (best effort) plus a local daily snapshot file for other tenors.
//
// Pure decision logic (expiry parsing, bucketing, rank, summary) is kept in
// standalone functions so tests stay hermetic (no network).

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const crCacheTTL = 60 * time.Second

// ---------------------------------------------------------------------------
// Response types (GET /api/v1/crypto-radar?symbol=BTC)
// ---------------------------------------------------------------------------

type crPricePoint struct {
	T     string  `json:"t"` // YYYY-MM-DD
	Price float64 `json:"price"`
}

type crIVBucket struct {
	Label   string   `json:"label"` // 1W | 1M | 3M | 6M
	DTE     float64  `json:"dte"`
	Expiry  string   `json:"expiry"` // YYYY-MM-DD of the matched Deribit expiry
	Current *float64 `json:"current"` // fraction, e.g. 0.3343
	WeekAgo *float64 `json:"week_ago"`
	Change  *float64 `json:"change"` // current - weekAgo, in points (0.01 = 1%)
}

type crRank struct {
	Value   *float64 `json:"value"`
	WeekAgo *float64 `json:"week_ago"`
	Change  *float64 `json:"change"`
	Min     *float64 `json:"min"`
	Max     *float64 `json:"max"`
	Count   int      `json:"count"`
}

type crSkewPoint struct {
	Strike float64 `json:"strike"`
	IV     float64 `json:"iv"` // fraction
}

type crSummary struct {
	Bullets []string `json:"bullets"`
	Detail  []string `json:"detail"`
}

type crRadarResponse struct {
	Symbol       string       `json:"symbol"`
	AsOf         string       `json:"as_of"`
	Spot         float64      `json:"spot"`
	SpotWeekAgo  *float64     `json:"spot_week_ago"`
	SpotChangePc *float64     `json:"spot_change_pct"`
	PriceHistory []crPricePoint `json:"price_history"`
	IVTable      []crIVBucket `json:"iv_table"`
	IVRank       crRank       `json:"iv_rank"`
	Skew         []crSkewPoint `json:"skew"`
	SkewATM      float64      `json:"skew_atm"`
	Summary      crSummary    `json:"summary"`
	Sources      map[string]string `json:"sources"`
}

// ---------------------------------------------------------------------------
// Pure helpers (hermetic, covered by tests)
// ---------------------------------------------------------------------------

var crMonths = map[string]time.Month{
	"JAN": time.January, "FEB": time.February, "MAR": time.March,
	"APR": time.April, "MAY": time.May, "JUN": time.June,
	"JUL": time.July, "AUG": time.August, "SEP": time.September,
	"OCT": time.October, "NOV": time.November, "DEC": time.December,
}

// crParseDeribitExpiry parses the Deribit expiry code like "26SEP25".
func crParseDeribitExpiry(code string) (time.Time, bool) {
	if len(code) < 6 {
		return time.Time{}, false
	}
	// Split trailing 2-digit year: DDMMMYY, day may be 1-2 digits.
	monPos := -1
	for i := 0; i < len(code); i++ {
		if code[i] >= 'A' && code[i] <= 'Z' {
			monPos = i
			break
		}
	}
	if monPos < 1 || monPos+3+2 > len(code) {
		return time.Time{}, false
	}
	day, err := strconv.Atoi(code[:monPos])
	if err != nil || day < 1 || day > 31 {
		return time.Time{}, false
	}
	mon, ok := crMonths[code[monPos:monPos+3]]
	if !ok {
		return time.Time{}, false
	}
	yy, err := strconv.Atoi(code[len(code)-2:])
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(2000+yy, mon, day, 8, 0, 0, 0, time.UTC), true
}

// crParseInstrument parses "BTC-26SEP25-90000-C" into expiry/strike/side.
func crParseInstrument(name string) (expiry time.Time, strike float64, isCall bool, ok bool) {
	parts := strings.Split(name, "-")
	if len(parts) != 4 {
		return time.Time{}, 0, false, false
	}
	exp, good := crParseDeribitExpiry(parts[1])
	if !good {
		return time.Time{}, 0, false, false
	}
	k, err := strconv.ParseFloat(parts[2], 64)
	if err != nil || k <= 0 {
		return time.Time{}, 0, false, false
	}
	switch parts[3] {
	case "C":
		return exp, k, true, true
	case "P":
		return exp, k, false, true
	default:
		return time.Time{}, 0, false, false
	}
}

// crNormIV converts a Deribit mark_iv quote to a fraction.
// Deribit quotes IV in percent (33.4 = 33.4%); values <= 2.5 are fractions.
func crNormIV(v float64) (float64, bool) {
	if v <= 0 || v > 500 {
		return 0, false
	}
	if v > 2.5 {
		v /= 100
	}
	if v < 0.03 || v > 5 {
		return 0, false
	}
	return v, true
}

func crMedian(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	m := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[m]
	}
	return (cp[m-1] + cp[m]) / 2
}

type crExpGroup struct {
	Expiry time.Time
	Key    string // YYYY-MM-DD
	DTE    float64
	Rows   []crOptRow
}

type crOptRow struct {
	Strike float64
	IsCall bool
	IV     float64 // fraction
}

// crGroupByExpiry groups option rows by expiry day and stamps DTE.
func crGroupByExpiry(rows []crOptRow, exps map[string]time.Time, now time.Time) map[string]*crExpGroup {
	out := map[string]*crExpGroup{}
	for k, e := range exps {
		out[k] = &crExpGroup{Expiry: e, Key: k, DTE: e.Sub(now).Hours() / 24}
	}
	for _, r := range rows {
		_ = r
	}
	return out
}

// crATMIv returns the median IV of near-ATM strikes of one expiry group.
func crATMIv(rows []crOptRow, spot float64) (float64, bool) {
	if len(rows) == 0 || spot <= 0 {
		return 0, false
	}
	atm := rows[0].Strike
	best := math.Abs(atm - spot)
	for _, r := range rows[1:] {
		if d := math.Abs(r.Strike - spot); d < best {
			best, atm = d, r.Strike
		}
	}
	var near []float64
	for _, r := range rows {
		if math.Abs(r.Strike-atm)/atm <= 0.03 {
			near = append(near, r.IV)
		}
	}
	if len(near) == 0 {
		return 0, false
	}
	return crMedian(near), true
}

// crPickBucket finds the expiry group closest to a target DTE.
func crPickBucket(groups []*crExpGroup, target float64) *crExpGroup {
	var best *crExpGroup
	bestScore := math.MaxFloat64
	for _, g := range groups {
		if g.DTE < 2 || g.DTE > 300 {
			continue
		}
		if s := math.Abs(g.DTE - target); s < bestScore {
			bestScore, best = s, g
		}
	}
	if best == nil {
		return nil
	}
	if math.Abs(best.DTE-target) > math.Max(6, target*0.6) {
		return nil
	}
	return best
}

// crIVRank computes IV-Rank against a trailing history window.
func crIVRank(current float64, hist []float64) (rank, mn, mx float64, ok bool) {
	if len(hist) < 10 || current <= 0 {
		return 0, 0, 0, false
	}
	mn, mx = hist[0], hist[0]
	for _, v := range hist[1:] {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	if mx <= mn {
		return 0, mn, mx, false
	}
	rank = (current - mn) / (mx - mn) * 100
	if rank < 0 {
		rank = 0
	}
	if rank > 100 {
		rank = 100
	}
	return rank, mn, mx, true
}

// crSkewAsymmetry compares average put-wing IV vs call-wing IV.
func crSkewAsymmetry(pts []crSkewPoint, spot float64) float64 {
	var putSum, callSum float64
	var pn, cn int
	for _, p := range pts {
		switch {
		case p.Strike < 0.97*spot:
			putSum += p.IV
			pn++
		case p.Strike > 1.03*spot:
			callSum += p.IV
			cn++
		}
	}
	if pn == 0 || cn == 0 {
		return 0
	}
	return putSum/float64(pn) - callSum/float64(cn)
}

func fptr(v float64) *float64 { return &v }

// crBuildSummary generates the 5 bullets + detail paragraphs (Russian).
func crBuildSummary(symbol string, spotChg, ivChgAvg float64, rank float64, rankOK bool, slope float64, asym float64, rangeLo, rangeHi float64) crSummary {
	dirWord := "выросла"
	if ivChgAvg < -0.001 {
		dirWord = "снизилась"
	} else if math.Abs(ivChgAvg) <= 0.001 {
		dirWord = "почти не изменилась"
	}
	b1 := fmt.Sprintf("После сильного движения цена %s торгуется в диапазоне %s–%s.", symbol, crFmtNum(rangeLo), crFmtNum(rangeHi))
	b2 := fmt.Sprintf("За неделю IV %s на большинстве сроков (среднее изменение %s п.п.).", dirWord, crFmtSigned(ivChgAvg*100))
	b3 := "IV-Rank: недостаточно истории — накапливаем ежедневные snapshots."
	if rankOK {
		level := "низкий — текущая IV у нижней границы годового диапазона"
		if rank > 70 {
			level = "высокий — текущая IV у верхней границы годового диапазона"
		} else if rank > 30 {
			level = "средний — текущая IV в середине годового диапазона"
		}
		b3 = fmt.Sprintf("IV-Rank %.0f%% — %s.", rank, level)
	}
	b4 := "Term Structure восходящая (контанго): повышенного спроса на краткосрочную волатильность нет."
	if slope < 0 {
		b4 = "Term Structure инвертирована (бэквордация): рынок доплачивает за краткосрочную волатильность."
	}
	b5 := "Выраженной асимметрии IV между Put- и Call-страйками не наблюдается."
	if asym > 0.02 {
		b5 = "Заметный перекос в сторону Put: защита от падения заметно дороже Call."
	} else if asym < -0.02 {
		b5 = "Заметный перекос в сторону Call: рынок доплачивает за рост."
	}
	detail := []string{
		fmt.Sprintf("За последнюю неделю IV %s, особенно на ближайших экспирациях. После сильного роста %s и перехода рынка в более спокойное состояние волатильность возвращается к уровням до предыдущего движения.", dirWord, symbol),
		"При этом IV-Rank остается на низком уровне, а краткосрочная IV продолжает снижаться. Это создает условия для поиска идей на покупку волатильности. Но важно помнить: низкая IV сама по себе не является сигналом к покупке. Ключевой вопрос — ожидаем ли мы новое движение и достаточно ли привлекательна текущая стоимость опционов относительно этого ожидания.",
	}
	_ = spotChg
	return crSummary{Bullets: []string{b1, b2, b3, b4, b5}, Detail: detail}
}

func crFmtNum(v float64) string {
	if v >= 1000 {
		return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func crFmtSigned(v float64) string {
	s := "+"
	if v < 0 {
		s = ""
	}
	return s + strconv.FormatFloat(math.Round(v*100)/100, 'f', 2, 64)
}

// ---------------------------------------------------------------------------
// Snapshot history (local file, one snapshot per symbol per day)
// ---------------------------------------------------------------------------

type crSnapshot struct {
	Date string             `json:"date"` // YYYY-MM-DD
	Spot float64            `json:"spot"`
	IV   map[string]float64 `json:"iv"` // label -> fraction
}

func crHistoryFile() string {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	return filepath.Join(dir, "crypto_radar_history.json")
}

var crHistMu sync.Mutex

func crLoadHistory() map[string][]crSnapshot {
	m := map[string][]crSnapshot{}
	data, err := os.ReadFile(crHistoryFile())
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

func crSaveHistory(m map[string][]crSnapshot) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(crHistoryFile()), 0o755)
	_ = os.WriteFile(crHistoryFile(), data, 0o644)
}

// crWeekAgoSnapshot returns the snapshot closest to 7 days ago (5..9d window).
func crWeekAgoSnapshot(snaps []crSnapshot, now time.Time) *crSnapshot {
	var best *crSnapshot
	bestScore := 1 << 30
	for i := range snaps {
		d, err := time.Parse("2006-01-02", snaps[i].Date)
		if err != nil {
			continue
		}
		age := int(now.Sub(d).Hours() / 24)
		if age < 5 || age > 9 {
			continue
		}
		if s := abs(age - 7); s < bestScore {
			bestScore = s
			best = &snaps[i]
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Live fetchers (network)
// ---------------------------------------------------------------------------

var crHTTP = &http.Client{Timeout: 10 * time.Second}

func crSymbolPair(symbol string) (pair, currency string, ok bool) {
	switch strings.ToUpper(symbol) {
	case "BTC":
		return "BTCUSDT", "BTC", true
	case "ETH":
		return "ETHUSDT", "ETH", true
	default:
		return "", "", false
	}
}

// crFetchSpotHistory tries Binance, then Bybit. Returns current, week-ago,
// daily closes (oldest first) and the source name.
func crFetchSpotHistory(symbol string) (cur float64, weekAgo float64, hist []crPricePoint, src string, err error) {
	pair, _, ok := crSymbolPair(symbol)
	if !ok {
		return 0, 0, nil, "", fmt.Errorf("unsupported crypto symbol %s", symbol)
	}
	if c, w, h, e := crBinanceKlines(pair); e == nil && c > 0 && len(h) > 8 {
		return c, w, h, "Binance", nil
	}
	if c, w, h, e := crBybitKlines(pair); e == nil && c > 0 && len(h) > 8 {
		return c, w, h, "Bybit", nil
	}
	return 0, 0, nil, "", fmt.Errorf("no spot history for %s", symbol)
}

func crBinanceKlines(pair string) (float64, float64, []crPricePoint, error) {
	url := fmt.Sprintf("https://api.binance.com/api/v3/klines?symbol=%s&interval=1d&limit=150", pair)
	resp, err := crHTTP.Get(url)
	if err != nil {
		return 0, 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, nil, fmt.Errorf("binance status %d", resp.StatusCode)
	}
	var rows [][]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return 0, 0, nil, err
	}
	var out []crPricePoint
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		ms, _ := r[0].(float64)
		closeStr, _ := r[4].(string)
		c, err := strconv.ParseFloat(closeStr, 64)
		if err != nil || c <= 0 {
			continue
		}
		out = append(out, crPricePoint{T: time.UnixMilli(int64(ms)).UTC().Format("2006-01-02"), Price: c})
	}
	if len(out) < 9 {
		return 0, 0, nil, fmt.Errorf("binance: too few rows")
	}
	cur := out[len(out)-1].Price
	week := out[len(out)-8].Price
	if len(out) > 120 {
		out = out[len(out)-120:]
	}
	return cur, week, out, nil
}

func crBybitKlines(pair string) (float64, float64, []crPricePoint, error) {
	url := fmt.Sprintf("https://api.bybit.com/v5/market/kline?category=spot&symbol=%s&interval=D&limit=150", pair)
	resp, err := crHTTP.Get(url)
	if err != nil {
		return 0, 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, nil, fmt.Errorf("bybit status %d", resp.StatusCode)
	}
	var wrap struct {
		RetCode int `json:"retCode"`
		Result  struct {
			List [][]string `json:"list"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return 0, 0, nil, err
	}
	if wrap.RetCode != 0 || len(wrap.Result.List) < 9 {
		return 0, 0, nil, fmt.Errorf("bybit: bad payload")
	}
	// Bybit returns newest first.
	rows := append([][]string(nil), wrap.Result.List...)
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	var out []crPricePoint
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		ms, _ := strconv.ParseInt(r[0], 10, 64)
		c, err := strconv.ParseFloat(r[4], 64)
		if err != nil || c <= 0 {
			continue
		}
		out = append(out, crPricePoint{T: time.UnixMilli(ms).UTC().Format("2006-01-02"), Price: c})
	}
	if len(out) < 9 {
		return 0, 0, nil, fmt.Errorf("bybit: too few rows")
	}
	cur := out[len(out)-1].Price
	week := out[len(out)-8].Price
	if len(out) > 120 {
		out = out[len(out)-120:]
	}
	return cur, week, out, nil
}

// crFetchDeribitIV fetches the option book summary and returns rows grouped
// by expiry day. mark_iv is normalized to a fraction.
func crFetchDeribitIV(currency string, now time.Time) (map[string]*crExpGroup, string, error) {
	url := fmt.Sprintf("https://www.deribit.com/api/v2/public/get_book_summary_by_currency?currency=%s&kind=option", currency)
	resp, err := crHTTP.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("deribit status %d", resp.StatusCode)
	}
	var wrap struct {
		Result []struct {
			InstrumentName string  `json:"instrument_name"`
			MarkIV         float64 `json:"mark_iv"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, "", err
	}
	if len(wrap.Result) == 0 {
		return nil, "", fmt.Errorf("deribit: empty book summary")
	}
	groups := map[string]*crExpGroup{}
	withIV := 0
	for _, r := range wrap.Result {
		exp, strike, isCall, ok := crParseInstrument(r.InstrumentName)
		if !ok || exp.Before(now.Add(-24*time.Hour)) {
			continue
		}
		iv, ok := crNormIV(r.MarkIV)
		if !ok {
			continue
		}
		withIV++
		key := exp.Format("2006-01-02")
		g, ok := groups[key]
		if !ok {
			g = &crExpGroup{Expiry: exp, Key: key, DTE: exp.Sub(now).Hours() / 24}
			groups[key] = g
		}
		g.Rows = append(g.Rows, crOptRow{Strike: strike, IsCall: isCall, IV: iv})
	}
	if withIV < 10 {
		return nil, "", fmt.Errorf("deribit: too few IV rows (%d)", withIV)
	}
	return groups, "Deribit", nil
}

// crFetchDVOLHistory is best-effort: trailing daily closes of the Deribit
// volatility index (30d implied vol, percent). Returns fraction series.
func crFetchDVOLHistory(currency string) ([]crPricePoint, error) {
	end := time.Now().UnixMilli()
	start := time.Now().AddDate(-1, 0, 0).UnixMilli()
	url := fmt.Sprintf("https://www.deribit.com/api/v2/public/get_volatility_index_data?currency=%s&start_timestamp=%d&end_timestamp=%d&resolution=1D",
		currency, start, end)
	resp, err := crHTTP.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dvol status %d", resp.StatusCode)
	}
	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	pts := crExtractTVSeries(raw)
	if len(pts) < 15 {
		return nil, fmt.Errorf("dvol: too few points")
	}
	return pts, nil
}

// crExtractTVSeries pulls [[ts,o,h,l,c],...] candles out of a defensive
// TradingView-shaped payload and returns daily closes as fractions.
func crExtractTVSeries(raw map[string]interface{}) []crPricePoint {
	var findCandles func(v interface{}) [][]interface{}
	findCandles = func(v interface{}) [][]interface{} {
		switch t := v.(type) {
		case map[string]interface{}:
			for _, k := range []string{"data", "candles", "result"} {
				if inner, ok := t[k]; ok {
					if c := findCandles(inner); c != nil {
						return c
					}
				}
			}
			// {ticks:[...], close:[...]} shape
			if ticks, ok := t["ticks"].([]interface{}); ok {
				if closes, ok := t["close"].([]interface{}); ok && len(ticks) == len(closes) {
					out := make([][]interface{}, len(ticks))
					for i := range ticks {
						out[i] = []interface{}{ticks[i], nil, nil, nil, closes[i]}
					}
					return out
				}
			}
		case []interface{}:
			if len(t) == 0 {
				return nil
			}
			if row, ok := t[0].([]interface{}); ok && len(row) >= 5 {
				out := make([][]interface{}, 0, len(t))
				for _, r := range t {
					if rr, ok := r.([]interface{}); ok && len(rr) >= 5 {
						out = append(out, rr)
					}
				}
				if len(out) > 0 {
					return out
				}
			}
		}
		return nil
	}
	candles := findCandles(raw)
	var out []crPricePoint
	for _, c := range candles {
		ts, _ := c[0].(float64)
		var close float64
		switch v := c[4].(type) {
		case float64:
			close = v
		case string:
			close, _ = strconv.ParseFloat(v, 64)
		}
		if ts <= 0 || close <= 0 {
			continue
		}
		// ms vs seconds
		if ts > 1e12 {
			ts /= 1000
		}
		iv, ok := crNormIV(close)
		if !ok {
			continue
		}
		out = append(out, crPricePoint{T: time.Unix(int64(ts), 0).UTC().Format("2006-01-02"), Price: iv})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

// ---------------------------------------------------------------------------
// Builder + handler with short cache
// ---------------------------------------------------------------------------

type crCacheEntry struct {
	resp crRadarResponse
	until time.Time
}

var (
	crCache   = map[string]crCacheEntry{}
	crCacheMu sync.Mutex
)

func cryptoRadarHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if symbol == "" {
		symbol = "BTC"
	}
	if _, _, ok := crSymbolPair(symbol); !ok {
		http.Error(w, "unsupported symbol (use BTC or ETH)", http.StatusBadRequest)
		return
	}
	crCacheMu.Lock()
	if e, ok := crCache[symbol]; ok && time.Now().Before(e.until) {
		crCacheMu.Unlock()
		json.NewEncoder(w).Encode(e.resp)
		return
	}
	crCacheMu.Unlock()

	resp, err := crBuildRadar(symbol)
	if err != nil {
		http.Error(w, "crypto radar unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	crCacheMu.Lock()
	crCache[symbol] = crCacheEntry{resp: resp, until: time.Now().Add(crCacheTTL)}
	crCacheMu.Unlock()
	json.NewEncoder(w).Encode(resp)
}

func crBuildRadar(symbol string) (crRadarResponse, error) {
	now := time.Now().UTC()
	_, currency, _ := crSymbolPair(symbol)

	spot, spotWeek, priceHist, spotSrc, err := crFetchSpotHistory(symbol)
	if err != nil || spot <= 0 {
		return crRadarResponse{}, fmt.Errorf("spot: %v", err)
	}
	groups, optSrc, err := crFetchDeribitIV(currency, now)
	if err != nil {
		return crRadarResponse{}, fmt.Errorf("options: %v", err)
	}
	list := make([]*crExpGroup, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}

	// Buckets 1W/1M/3M/6M.
	targets := []struct {
		label string
		dte   float64
	}{{"1W", 7}, {"1M", 30}, {"3M", 90}, {"6M", 180}}
	type bucketLive struct {
		label string
		dte   float64
		exp   string
		iv    float64
		ok    bool
	}
	var lives []bucketLive
	curByLabel := map[string]float64{}
	for _, t := range targets {
		g := crPickBucket(list, t.dte)
		if g == nil {
			lives = append(lives, bucketLive{label: t.label})
			continue
		}
		iv, ok := crATMIv(g.Rows, spot)
		if !ok {
			lives = append(lives, bucketLive{label: t.label})
			continue
		}
		lives = append(lives, bucketLive{label: t.label, dte: g.DTE, exp: g.Key, iv: iv, ok: true})
		curByLabel[t.label] = iv
	}
	if len(curByLabel) < 2 {
		return crRadarResponse{}, fmt.Errorf("deribit: no usable expiries")
	}

	// Week-ago IV: prefer the local snapshot, else scale from DVOL history.
	crHistMu.Lock()
	histMap := crLoadHistory()
	snaps := histMap[symbol]
	weekSnap := crWeekAgoSnapshot(snaps, now)
	crHistMu.Unlock()

	weekByLabel := map[string]float64{}
	if weekSnap != nil {
		for k, v := range weekSnap.IV {
			weekByLabel[k] = v
		}
	} else if dvol, err := crFetchDVOLHistory(currency); err == nil && len(dvol) > 8 {
		dvolWeek := dvol[len(dvol)-8].Price
		if cur1M, ok := curByLabel["1M"]; ok && cur1M > 0 {
			for _, b := range lives {
				if !b.ok {
					continue
				}
				weekByLabel[b.label] = dvolWeek * (b.iv / cur1M)
			}
		}
	}

	// IV-Rank on the 1M leg: DVOL window preferred, snapshot window fallback.
	var rankVal, rankWeek, rankChg, rankMin, rankMax *float64
	rankCount := 0
	if cur1M, ok := curByLabel["1M"]; ok {
		if dvol, err := crFetchDVOLHistory(currency); err == nil && len(dvol) >= 30 {
			var win []float64
			for _, p := range dvol {
				win = append(win, p.Price)
			}
			if rv, mn, mx, ok := crIVRank(cur1M, win); ok {
				rankVal, rankMin, rankMax = fptr(rv), fptr(mn), fptr(mx)
				rankCount = len(win)
				if len(dvol) > 8 {
					w1m := dvol[len(dvol)-8].Price
					if rw, _, _, ok := crIVRank(w1m, win); ok {
						rankWeek = fptr(rw)
						rankChg = fptr(rv - rw)
					}
				}
			}
		}
		if rankVal == nil && len(snaps) >= 10 {
			var win []float64
			for _, s := range snaps {
				if v, ok := s.IV["1M"]; ok && v > 0 {
					win = append(win, v)
				}
			}
			win = append(win, cur1M)
			if rv, mn, mx, ok := crIVRank(cur1M, win); ok {
				rankVal, rankMin, rankMax = fptr(rv), fptr(mn), fptr(mx)
				rankCount = len(win)
			}
		}
	}

	// Skew on the front expiry (nearest >= 5d).
	var front *crExpGroup
	for _, g := range list {
		if g.DTE < 5 {
			continue
		}
		if front == nil || g.DTE < front.DTE {
			front = g
		}
	}
	var skew []crSkewPoint
	atmStrike := 0.0
	asym := 0.0
	if front != nil {
		byStrike := map[float64][]float64{}
		for _, row := range front.Rows {
			if math.Abs(row.Strike-spot)/spot > 0.15 {
				continue
			}
			byStrike[row.Strike] = append(byStrike[row.Strike], row.IV)
		}
		type kv struct {
			k float64
			v float64
		}
		var arr []kv
		for k, vs := range byStrike {
			arr = append(arr, kv{k, crMedian(vs)})
		}
		sort.Slice(arr, func(i, j int) bool { return arr[i].k < arr[j].k })
		// Evenly sample up to 11 points.
		if len(arr) > 11 {
			step := float64(len(arr)-1) / 10
			var sampled []kv
			for i := 0; i < 11; i++ {
				sampled = append(sampled, arr[int(math.Round(float64(i)*step))])
			}
			arr = sampled
		}
		best := math.MaxFloat64
		for _, e := range arr {
			skew = append(skew, crSkewPoint{Strike: e.k, IV: e.v})
			if d := math.Abs(e.k - spot); d < best {
				best, atmStrike = d, e.k
			}
		}
		asym = crSkewAsymmetry(skew, spot)
	}

	// Assemble table + averages for the summary.
	var table []crIVBucket
	var chgSum float64
	var chgN int
	for _, b := range lives {
		row := crIVBucket{Label: b.label, DTE: math.Round(b.dte*10) / 10, Expiry: b.exp}
		if b.ok {
			v := b.iv
			row.Current = fptr(v)
			if w, ok := weekByLabel[b.label]; ok && w > 0 {
				row.WeekAgo = fptr(w)
				row.Change = fptr(v - w)
				chgSum += v - w
				chgN++
			}
		}
		table = append(table, row)
	}
	ivChgAvg := 0.0
	if chgN > 0 {
		ivChgAvg = chgSum / float64(chgN)
	}
	lo, hi := spot, spot
	for _, p := range priceHist {
		if p.Price < lo {
			lo = p.Price
		}
		if p.Price > hi {
			hi = p.Price
		}
	}
	slope := 0.0
	if m6, ok := curByLabel["6M"]; ok {
		if w1, ok := curByLabel["1W"]; ok {
			slope = m6 - w1
		} else if m1, ok := curByLabel["1M"]; ok {
			slope = m6 - m1
		}
	}
	rankNum, rankOK := 0.0, false
	if rankVal != nil {
		rankNum, rankOK = *rankVal, true
	}
	summary := crBuildSummary(symbol, 0, ivChgAvg, rankNum, rankOK, slope, asym, lo, hi)

	var spotChg *float64
	if spotWeek > 0 {
		spotChg = fptr((spot - spotWeek) / spotWeek * 100)
	}

	// Persist today's snapshot (best effort).
	crHistMu.Lock()
	func() {
		defer crHistMu.Unlock()
		m := crLoadHistory()
		today := now.Format("2006-01-02")
		ss := m[symbol]
		if len(ss) > 0 && ss[len(ss)-1].Date == today {
			return
		}
		ivm := map[string]float64{}
		for _, b := range lives {
			if b.ok {
				ivm[b.label] = b.iv
			}
		}
		if len(ivm) == 0 {
			return
		}
		ss = append(ss, crSnapshot{Date: today, Spot: spot, IV: ivm})
		if len(ss) > 400 {
			ss = ss[len(ss)-400:]
		}
		m[symbol] = ss
		crSaveHistory(m)
	}()

	return crRadarResponse{
		Symbol:         symbol,
		AsOf:           now.Format("2006-01-02"),
		Spot:           spot,
		SpotWeekAgo:    fptr(spotWeek),
		SpotChangePc:   spotChg,
		PriceHistory:   priceHist,
		IVTable:        table,
		IVRank:         crRank{Value: rankVal, WeekAgo: rankWeek, Change: rankChg, Min: rankMin, Max: rankMax, Count: rankCount},
		Skew:           skew,
		SkewATM:        atmStrike,
		Summary:        summary,
		Sources:        map[string]string{"spot": spotSrc, "options": optSrc},
	}, nil
}
