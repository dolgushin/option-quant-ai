package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"option-quant-ai/quant"
)

// ivHistoryPoint is one sampled daily ATM implied volatility for an asset.
type ivHistoryPoint struct {
	Date string  `json:"date"` // YYYY-MM-DD
	IV   float64 `json:"iv"`   // decimal, clamped [0.20, 0.80]
}

// ivHistoryState caches sampled historical ATM IV per asset code.
type ivHistoryState struct {
	mu      sync.RWMutex
	byAsset map[string][]ivHistoryPoint
	built   map[string]bool
	buildMu sync.Mutex
}

var ivHistory = &ivHistoryState{byAsset: map[string][]ivHistoryPoint{}, built: map[string]bool{}}

func ivHistoryFile() string {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	return filepath.Join(dir, "iv_history.json")
}

func loadIVHistory() {
	data, err := os.ReadFile(ivHistoryFile())
	if err != nil {
		return
	}
	var m map[string][]ivHistoryPoint
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	ivHistory.mu.Lock()
	for k, v := range m {
		ivHistory.byAsset[k] = v
	}
	ivHistory.mu.Unlock()
}

func saveIVHistory() {
	ivHistory.mu.RLock()
	m := make(map[string][]ivHistoryPoint, len(ivHistory.byAsset))
	for k, v := range ivHistory.byAsset {
		cp := make([]ivHistoryPoint, len(v))
		copy(cp, v)
		m[k] = cp
	}
	ivHistory.mu.RUnlock()
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	if dir := filepath.Dir(ivHistoryFile()); dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	os.WriteFile(ivHistoryFile(), data, 0o644)
}

// issAssetCode maps our UI symbol to the MOEX options ASSETCODE.
func issAssetCode(symbol string) string {
	switch symbol {
	case "RI":
		return "RTS"
	case "CR":
		return "CNY"
	default:
		return "Si"
	}
}

var secidRe = regexp.MustCompile(`^([A-Za-z]+)([0-9.]+)([A-Z])([A-Z])([0-9])$`)

// decodedSecid holds the fields parsed from a MOEX options SECID.
type decodedSecid struct {
	Strike float64
	IsCall bool
	Expiry time.Time // 3rd Thursday of the decoded month
}

// decodeSecid parses a MOEX option SECID like Si84000BI6.
// Format: <asset><strike><B><type-month-letter><year-digit>.
// Letters A-L (1-12) are calls (month = letter index), letters M-Z (13-26)
// are puts (month = letter index - 12). The year digit is the last digit of
// the expiry year, interpreted relative to the current date.
func decodeSecid(secid string) (*decodedSecid, error) {
	m := secidRe.FindStringSubmatch(secid)
	if m == nil {
		return nil, fmt.Errorf("cannot parse secid %s", secid)
	}
	strike, err := strconv.ParseFloat(m[2], 64)
	if err != nil || strike <= 0 {
		return nil, fmt.Errorf("bad strike in %s", secid)
	}
	letter := m[4][0]
	pos := int(letter - 'A' + 1) // 1..26
	digit := int(m[5][0] - '0')

	// Interpret the year digit relative to the current decade.
	curYear := time.Now().Year()
	base := curYear - (curYear % 10) + digit
	if base < curYear-1 {
		base += 10
	}
	if base > curYear+1 {
		base -= 10
	}

	isCall := pos <= 12
	month := pos
	if !isCall {
		month = pos - 12
	}
	// MOEX options expire on the 3rd Thursday of the month.
	exp := thirdThursday(base, month)
	return &decodedSecid{Strike: strike, IsCall: isCall, Expiry: exp}, nil
}

// thirdThursday returns the third Thursday of the given month/year.
func thirdThursday(year, month int) time.Time {
	first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	// First Thursday of the month: weekday offset.
	offset := (time.Thursday - first.Weekday() + 7) % 7
	firstThu := first.AddDate(0, 0, int(offset))
	return firstThu.AddDate(0, 0, 14) // third Thursday
}

// fetchHistoricalBoard fetches the dated history board for one trading date and
// keeps only rows for the target asset. The securities board with a date param
// ignores the date, so we use the history endpoint (paginated at 100 rows).
// SETTLEPRICE is the official settlement price, populated for every contract
// (CLOSE is null for untraded strikes).
func fetchHistoricalBoard(asset string, date string) ([][]interface{}, error) {
	var out [][]interface{}
	for start := 0; ; start += 100 {
		url := fmt.Sprintf("http://iss.moex.com/iss/history/engines/futures/markets/options/boards/RFUD/securities.json?iss.meta=off&date=%s&assetcode=%s&iss.only=history&history.columns=SECID,SETTLEPRICE&start=%d&limit=100", date, asset, start)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			return nil, err
		}
		var data struct {
			History struct {
				Data [][]interface{} `json:"data"`
			} `json:"history"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		rows := data.History.Data
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if len(row) < 2 {
				continue
			}
			secid, _ := row[0].(string)
			settle, _ := row[1].(float64)
			if secid == "" || settle <= 0 {
				continue
			}
			if assetPrefixMatch(asset, secid) {
				out = append(out, row)
			}
		}
	}
	return out, nil
}

// assetPrefixMatch reports whether a SECID belongs to the given MOEX asset code.
// The history API filters by assetcode (Si/RTS/CNY) but SECIDs use the futures
// prefix (Si/RI/CNY).
func assetPrefixMatch(asset, secid string) bool {
	prefix := secidRe.FindStringSubmatch(secid)
	if prefix == nil || len(prefix) < 2 {
		return false
	}
switch asset {
	case "RTS":
		return prefix[1] == "RI"
	case "CNY":
		return prefix[1] == "CR"
	default:
		return prefix[1] == "Si"
	}
}

// assetCodeFromSecid extracts the asset code prefix from a SECID.
func assetCodeFromSecid(secid string) string {
	m := secidRe.FindStringSubmatch(secid)
	if m == nil {
		return ""
	}
	return m[1]
}

// historicalATMIV computes the ATM implied vol for an asset on a given trading
// date using the dated history board and SECID decoding.
func historicalATMIV(asset string, date string) float64 {
	rows, err := fetchHistoricalBoard(asset, date)
	if err != nil || len(rows) < 2 {
		return 0
	}
	ref, _ := time.Parse("2006-01-02", date)

	type opt struct {
		dec   *decodedSecid
		price float64
	}
	var opts []opt
	for _, r := range rows {
		secid, _ := r[0].(string)
		price, _ := r[1].(float64)
		dec, err := decodeSecid(secid)
		if err != nil || price <= 0 {
			continue
		}
		opts = append(opts, opt{dec: dec, price: price})
	}
	if len(opts) < 2 {
		return 0
	}

	// Pick the expiry closest to ~40 DTE within [15, 80].
	bestExp := time.Time{}
	bestScore := 1 << 30
	for _, o := range opts {
		d := int(o.dec.Expiry.Sub(ref).Hours() / 24)
		if d < 15 || d > 80 {
			continue
		}
		score := int(math.Abs(float64(d - 40)))
		if score < bestScore {
			bestScore = score
			bestExp = o.dec.Expiry
		}
	}
	if bestExp.IsZero() {
		return 0
	}

	// Underlying futures close on that date via the quarterly futures series
	// matching the option's expiry month. Fall back to the nearest ATM strike
	// interpolation if futures data is unavailable.
	und := underlyingClose(asset, ref)
	if und <= 0 {
		return 0
	}

	// Find the strike closest to the underlying price among options of the
	// chosen expiry, then average the call and put IV at that strike.
	type pair struct {
		call, put float64
	}
	byStrike := map[float64]*pair{}
	var strikes []float64
	for _, o := range opts {
		if !o.dec.Expiry.Equal(bestExp) {
			continue
		}
		p := byStrike[o.dec.Strike]
		if p == nil {
			p = &pair{}
			byStrike[o.dec.Strike] = p
			strikes = append(strikes, o.dec.Strike)
		}
		if o.dec.IsCall {
			p.call = o.price
		} else {
			p.put = o.price
		}
	}
	if len(strikes) == 0 {
		return 0
	}
	atmStrike := 0.0
	bestDiff := 1 << 30
	for _, s := range strikes {
		d := int(math.Abs(s - und))
		if d < bestDiff {
			bestDiff = d
			atmStrike = s
		}
	}
	p := byStrike[atmStrike]
	if p == nil || p.call <= 0 || p.put <= 0 {
		return 0
	}
	days := int(bestExp.Sub(ref).Hours() / 24)
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0
	rRate := 0.16

	ivC := quant.ImpliedVolatility(true, p.call, und, atmStrike, t, rRate)
	ivP := quant.ImpliedVolatility(false, p.put, und, atmStrike, t, rRate)
	var ivSum float64
	n := 0
	if ivC > 0 {
		ivSum += ivC
		n++
	}
	if ivP > 0 {
		ivSum += ivP
		n++
	}
	if n == 0 {
		return 0
	}
iv := ivSum / float64(n)
	return math.Round(iv*10000) / 10000
}

// underlyingClose returns the futures close for the quarterly series that is
// the underlying of the asset's options on the given date.
func underlyingClose(asset string, ref time.Time) float64 {
	// Map asset to its quarterly futures codes for the relevant year(s).
	var codes []string
	switch asset {
	case "Si":
		codes = []string{"SiZ5", "SiH6", "SiM6", "SiU6", "SiZ6"}
	case "RTS":
		codes = []string{"RIZ5", "RIH6", "RIM6", "RIU6", "RIZ6"}
	case "CNY":
		codes = []string{"CRZ5", "CRH6", "CRM6", "CRU6", "CRZ6"}
	default:
		return 0
	}
	from := ref.AddDate(0, 0, -5).Format("2006-01-02")
	till := ref.AddDate(0, 0, 5).Format("2006-01-02")
	for _, code := range codes {
		candles, err := fetchFutureHistory(code, from, till)
		if err != nil || len(candles) == 0 {
			continue
		}
		// Find the candle for the reference date (or the closest prior one).
		var best float64
		for _, c := range candles {
			if c.Date.After(ref) {
				break
			}
			best = c.Close
		}
		if best > 0 {
			return best
		}
	}
	return 0
}

// ensureIVHistory builds the sample history for an asset once. It samples every
// ~5 weekdays over the past 365 days, skipping dates already cached, fetching
// boards concurrently.
func ensureIVHistory(symbol string) {
	asset := issAssetCode(symbol)
	ivHistory.buildMu.Lock()
	if ivHistory.built[symbol] {
		ivHistory.buildMu.Unlock()
		return
	}
	ivHistory.buildMu.Unlock()

	ivHistory.mu.RLock()
	existing := map[string]bool{}
	for _, p := range ivHistory.byAsset[asset] {
		existing[p.Date] = true
	}
	ivHistory.mu.RUnlock()

	// Build the list of candidate sample dates (weekdays only, ~5-day spacing).
	now := time.Now()
	var dates []string
	for d := now.AddDate(0, 0, -365); d.Before(now); d = d.AddDate(0, 0, 5) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		ds := d.Format("2006-01-02")
		if !existing[ds] {
			dates = append(dates, ds)
		}
	}
	if len(dates) == 0 {
		ivHistory.mu.Lock()
		ivHistory.built[symbol] = true
		ivHistory.mu.Unlock()
		return
	}

	// Concurrent fetch with a small worker pool.
	type result struct {
		date string
		iv   float64
	}
	results := make(chan result, len(dates))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, ds := range dates {
		wg.Add(1)
		go func(ds string) {
			defer wg.Done()
			sem <- struct{}{}
			iv := historicalATMIV(asset, ds)
			<-sem
			if iv > 0 {
				results <- result{date: ds, iv: iv}
			}
		}(ds)
	}
	wg.Wait()
	close(results)

	ivHistory.mu.Lock()
	for r := range results {
		ivHistory.byAsset[asset] = append(ivHistory.byAsset[asset], ivHistoryPoint{Date: r.date, IV: r.iv})
	}
	ivHistory.built[symbol] = true
	ivHistory.mu.Unlock()

	ivHistory.mu.RLock()
	pts := ivHistory.byAsset[asset]
	ivHistory.mu.RUnlock()
	sort.Slice(pts, func(i, j int) bool { return pts[i].Date < pts[j].Date })
	saveIVHistory()
}

// ivRankStats computes IV Rank / Percentile for the trailing year of samples.
func ivRankStats(symbol string, currentIV float64) map[string]interface{} {
	asset := issAssetCode(symbol)
	ensureIVHistory(symbol)
	ivHistory.mu.RLock()
	pts := ivHistory.byAsset[asset]
	ivHistory.mu.RUnlock()

	out := map[string]interface{}{
		"available":    false,
		"current_iv":   currentIV,
		"count":        0,
		"iv_rank":      nil,
		"iv_percentile": nil,
	}
	cutoff := time.Now().AddDate(0, 0, -365)
	var ivs []float64
	for _, p := range pts {
		d, err := time.Parse("2006-01-02", p.Date)
		if err != nil {
			continue
		}
		if d.After(cutoff) {
			ivs = append(ivs, p.IV)
		}
	}
	if len(ivs) < 10 {
		return out
	}
	sorted := make([]float64, len(ivs))
	copy(sorted, ivs)
	sort.Float64s(sorted)
	min, max := sorted[0], sorted[len(sorted)-1]
	rank := 0.0
	if max > min {
		rank = (currentIV - min) / (max - min) * 100
	}
	if rank < 0 {
		rank = 0
	}
	if rank > 100 {
		rank = 100
	}
	below := 0
	for _, v := range ivs {
		if v <= currentIV {
			below++
		}
	}
	percentile := float64(below) / float64(len(ivs)) * 100

	out["available"] = true
	out["count"] = len(ivs)
	out["min_iv"] = math.Round(min*10000) / 10000
	out["max_iv"] = math.Round(max*10000) / 10000
	out["median_iv"] = math.Round(sorted[len(sorted)/2]*10000) / 10000
	out["iv_rank"] = math.Round(rank)
	out["iv_percentile"] = math.Round(percentile)
	return out
}
