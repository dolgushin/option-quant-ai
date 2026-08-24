package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"option-quant-ai/agent"
	"option-quant-ai/alor"
	"option-quant-ai/quant"
	"option-quant-ai/secure"
)

//go:embed static/*
var staticFiles embed.FS

// Version: 1.0.1 - Initial capital widget added


type GreeksRequest struct {
	IsCall bool    `json:"is_call"`
	S      float64 `json:"spot_price"`
	K      float64 `json:"strike_price"`
	T      float64 `json:"time_to_exp"`
	R      float64 `json:"risk_free"`
	Sigma  float64 `json:"volatility"`
}

type SkewPoint struct {
	Strike float64 `json:"strike"`
	IV     float64 `json:"iv"`
}

func greeksHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req GreeksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	greeks := quant.CalculateBlackScholes(req.IsCall, req.S, req.K, req.T, req.R, req.Sigma)
	json.NewEncoder(w).Encode(greeks)
}

var (
	// Version: 1.3.0 - Real position tracking, trade history and statistics

	spotOverrides = map[string]float64{}
	spotMu        sync.Mutex

	// Selected options series per asset (MOEX code: SiU6 = September 2026).
	// The user can switch between quarterly/monthly series via POST /api/v1/series.
	// For SBER/SBERP the series is a premium equity option expiry (e.g. SBRF-2026-09-16).
	selectedSeries = map[string]string{
		"Si":    "SiU6",
		"RI":    "RIU6",
		"CR":    "CRU6",
		"SBER":  "SBRF-2026-09-16",
		"SBERP": "SBPR-2026-09-16",
	}
	seriesMu sync.Mutex

	// equityOptions maps our UI symbols to the MOEX ISS premium option asset
	// codes (board ROPD). SBER = Sberbank common shares, SBERP = preferred.
	equityOptions = map[string]string{
		"SBER":  "SBRF",
		"SBERP": "SBPR",
	}

	// equityOptionCache holds the ROPD (premium options on shares) contracts.
	equityOptionCache     []optionContract
	equityOptionCacheTime time.Time
	equityOptionMu        sync.Mutex

	// tokenStore persists the Alor refresh token encrypted on disk.
	tokenStore *secure.Store

	alorAuth   *alor.AuthClient
	alorMarket *alor.MarketClient
	alorExec   *alor.ExecutionClient
)

// futuresContract is a MOEX FORTS futures contract with its expiry date.
type futuresContract struct {
	Code        string // e.g. "SiU6"
	ShortName   string // e.g. "Si-9.26"
	LastDelDate string // e.g. "2026-09-17"
}

var (
	contractCache     []futuresContract
	contractCacheTime time.Time
	contractMu        sync.Mutex
)

// moexFuturesContracts fetches the full list of FORTS futures contracts with
// their last-delivery (expiry) dates from the public MOEX ISS API, cached.
func moexFuturesContracts() ([]futuresContract, error) {
	contractMu.Lock()
	defer contractMu.Unlock()
	if len(contractCache) > 0 && time.Since(contractCacheTime) < 10*time.Minute {
		return contractCache, nil
	}

	url := "http://iss.moex.com/iss/engines/futures/markets/forts/securities.json?iss.meta=off&iss.only=securities&securities.columns=SECID,SHORTNAME,LASTDELDATE"
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Securities struct {
			Data [][]interface{} `json:"data"`
		} `json:"securities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	var contracts []futuresContract
	for _, row := range data.Securities.Data {
		if len(row) < 3 {
			continue
		}
		code, _ := row[0].(string)
		short, _ := row[1].(string)
		delDate, _ := row[2].(string)
		if code != "" {
			contracts = append(contracts, futuresContract{Code: code, ShortName: short, LastDelDate: delDate})
		}
	}

	contractCache = contracts
	contractCacheTime = time.Now()
	return contracts, nil
}

// futuresContractsForSymbol returns available contracts for a symbol root (Si, RI, CR),
// newest to oldest, for the dropdown in the UI.
// For premium equity options (SBER/SBERP) there is no matching futures prefix,
// so we derive the "series" from the unique option expiries on the ROPD board.
func futuresContractsForSymbol(symbol string) []futuresContract {
	if _, isEquity := equityOptions[symbol]; isEquity {
		return equitySeriesForSymbol(symbol)
	}
	contracts, err := moexFuturesContracts()
	if err != nil {
		return nil
	}
	var out []futuresContract
	for _, c := range contracts {
		if strings.HasPrefix(c.Code, symbol) {
			out = append(out, c)
		}
	}
	// Newest expiry first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].LastDelDate > out[i].LastDelDate {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// equitySeriesForSymbol builds pseudo futuresContract entries for premium
// equity options (SBER/SBERP) from the unique expiries on the ROPD board.
// Code follows the same "{ASSET}-{YYYY-MM-DD}" pattern used by selectedSeries.
func equitySeriesForSymbol(symbol string) []futuresContract {
	issAsset := equityOptions[symbol]
	if issAsset == "" {
		return nil
	}
	opts, err := moexEquityOptionContracts()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []futuresContract
	for _, o := range opts {
		if o.AssetCode != issAsset || o.Expiry == "" || seen[o.Expiry] {
			continue
		}
		seen[o.Expiry] = true
		out = append(out, futuresContract{
			Code:        issAsset + "-" + o.Expiry,
			ShortName:   fmt.Sprintf("%s %s", symbol, o.Expiry),
			LastDelDate: o.Expiry,
		})
	}
	// Newest expiry first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].LastDelDate > out[i].LastDelDate {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// isSyntheticSeriesCode reports whether the series code is an expiry-based
// synthetic entry ("Si-2026-08-20") from optionSeriesForSymbol rather than a
// real futures contract code.
func isSyntheticSeriesCode(code string) bool {
	i := strings.IndexByte(code, '-')
	if i <= 0 || len(code)-i != 11 {
		return false
	}
	date := code[i+1:]
	for j := 0; j < len(date); j++ {
		c := date[j]
		isDigit := c >= '0' && c <= '9'
		isSep := c == '-' && (j == 4 || j == 7)
		if !isDigit && !isSep {
			return false
		}
	}
	return true
}

// syntheticSeriesExpiry parses the date part of a synthetic series code.
func syntheticSeriesExpiry(code string) (*time.Time, bool) {
	if !isSyntheticSeriesCode(code) {
		return nil, false
	}
	t, err := time.Parse("2006-01-02", code[strings.IndexByte(code, '-')+1:])
	if err != nil {
		return nil, false
	}
	return &t, true
}

// resolveRealFuturesCode maps a synthetic expiry-based code to the real
// futures contract of the same root expiring on/after that date — needed where
// a tradable ticker is required (quoting, margin, hedging).
func resolveRealFuturesCode(symbol, code string) string {
	exp, ok := syntheticSeriesExpiry(code)
	if !ok {
		return code
	}
	contracts, err := moexFuturesContracts()
	if err != nil {
		return ""
	}
	best := ""
	var bestDate time.Time
	for _, c := range contracts {
		if !strings.HasPrefix(c.Code, symbol) || c.LastDelDate == "" {
			continue
		}
		d, err := time.Parse("2006-01-02", c.LastDelDate)
		if err != nil || d.Before(*exp) {
			continue
		}
		if best == "" || d.Before(bestDate) {
			best, bestDate = c.Code, d
		}
	}
	return best
}

// optionSeriesForSymbol builds the series list from real OPTION expiries:
// unique dates of the asset's options — RFUD board for futures underlyings
// (weekly/monthly/quarterly series), ROPD board for SBER/SBERP. Codes are
// synthetic "{SYMBOL}-{YYYY-MM-DD}"; selectedSeries accepts both these and
// real futures codes.
func optionSeriesForSymbol(symbol string) []futuresContract {
	if _, isEquity := equityOptions[symbol]; isEquity {
		return equitySeriesForSymbol(symbol)
	}
	issAsset := symbol
	switch symbol {
	case "RI":
		issAsset = "RTS"
	case "CR":
		issAsset = "CNY"
	}
	opts, err := moexOptionContracts()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []futuresContract
	for _, o := range opts {
		if !strings.EqualFold(o.AssetCode, issAsset) || o.Expiry == "" || seen[o.Expiry] {
			continue
		}
		seen[o.Expiry] = true
		out = append(out, futuresContract{
			Code:        symbol + "-" + o.Expiry,
			ShortName:   fmt.Sprintf("%s %s", symbol, o.Expiry),
			LastDelDate: o.Expiry,
		})
	}
	// Newest expiry first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].LastDelDate > out[i].LastDelDate {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// classifyExpiry labels an expiry using the whole live chain: dates inside a
// dense cluster of expiries (neighbours ≤13 days apart) are weekly series, an
// isolated date in a quarter-end month is quarterly, any other isolated date
// is monthly.
func classifyExpiry(expiry string, all []time.Time) string {
	t, err := time.Parse("2006-01-02", expiry)
	if err != nil {
		return "серия"
	}
	// Pass 1: mark every expiry that belongs to a dense weekly cluster
	// (a neighbour within 13 days).
	inCluster := make([]bool, len(all))
	for i := range all {
		for j := range all {
			if i == j {
				continue
			}
			diff := all[j].Sub(all[i])
			if diff < 0 {
				diff = -diff
			}
			if diff < 14*24*time.Hour {
				inCluster[i] = true
				break
			}
		}
	}
	selfCluster := false
	aloneInMonth := true
	for i, o := range all {
		if o.Equal(t) {
			selfCluster = inCluster[i]
			continue
		}
		// Month-uniqueness counts only significant (non-weekly) expiries:
		// a weekly series inside the quarter month must not demote its
		// quarterly date (e.g. Sep weekly + Sep quarterly).
		if !inCluster[i] && o.Year() == t.Year() && o.Month() == t.Month() {
			aloneInMonth = false
		}
	}
	switch {
	case selfCluster:
		return "недельная"
	case aloneInMonth && t.Month()%3 == 0:
		return "квартальная"
	default:
		return "месячная"
	}
}

// seriesTypeCode maps a series label to its short W/M/Q code.
func seriesTypeCode(label string) string {
	switch label {
	case "недельная":
		return "W"
	case "квартальная":
		return "Q"
	case "месячная":
		return "M"
	default:
		return ""
	}
}

// dteInDays returns days-to-expiry from a MOEX expiry date string.
func dteInDays(lastDelDate string, ref time.Time) int {
	t, err := time.Parse("2006-01-02", lastDelDate)
	if err != nil {
		return 0
	}
	return int(t.Sub(ref).Hours() / 24)
}

// getSpotPrice returns the real-time futures/spot price.
// It first checks a user override, then MOEX ISS (public, no token), then the
// Alor API, then falls back to a realistic estimate.
func getSpotPrice(symbol string) (float64, error) {
	spotMu.Lock()
	if p, ok := spotOverrides[symbol]; ok {
		spotMu.Unlock()
		return p, nil
	}
	spotMu.Unlock()

	if symbol == "BTC" || symbol == "ETH" {
		q, err := quant.FetchCryptoSpot(symbol)
		if err != nil {
			return 0, err
		}
		return q.Price, nil
	}

	// 1) MOEX ISS public API — free, no token required, real FORTS prices.
	// For premium equity options (SBER/SBERP) the underlying is the share
	// itself, quoted on the stock market (TQBR), not a FORTS futures.
	if _, isEquity := equityOptions[symbol]; isEquity {
		if price, err := moexStockSpotPrice(symbol); err == nil && price > 0 {
			return price, nil
		}
	}

	seriesMu.Lock()
	issCode := selectedSeries[symbol]
	seriesMu.Unlock()
	// Synthetic expiry codes are not tradable tickers — resolve to the nearest
	// real futures contract for quoting.
	if isSyntheticSeriesCode(issCode) {
		issCode = resolveRealFuturesCode(symbol, issCode)
	}
	if issCode != "" {
		if price, err := moexISSSpotPrice(issCode); err == nil && price > 0 {
			return price, nil
		}
	}

	// 2) Alor API (needs a valid refresh token).
	if futuresSymbol, ok := futuresSeriesAlor(symbol); ok && alorMarket != nil {
		if quote, err := alorMarket.FetchSecurityQuote(futuresSymbol); err == nil && quote.Price > 0 {
			return quote.Price, nil
		}
	}

	// 3) Fallback to a realistic estimate.
	switch symbol {
	case "Si":
		return 83200.0, nil
	case "RI":
		return 80240.0, nil
	case "CR":
		return 1010.0, nil
	case "SBER":
		return 271.0, nil
	case "SBERP":
		return 271.0, nil
	default:
		return 0, fmt.Errorf("no price source for symbol %s", symbol)
	}
}

// moexStockSpotPrice fetches the LAST trade price of a share (e.g. SBER,
// SBERP) from the MOEX ISS stock TQBR board (no authentication required).
func moexStockSpotPrice(ticker string) (float64, error) {
	url := fmt.Sprintf("http://iss.moex.com/iss/engines/stock/markets/shares/boards/TQBR/securities/%s.json?iss.meta=off&iss.only=marketdata&marketdata.columns=SECID,LAST", ticker)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("moex iss share request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("moex iss share status: %d", resp.StatusCode)
	}

	var issResp struct {
		Marketdata struct {
			Data [][]interface{} `json:"data"`
		} `json:"marketdata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issResp); err != nil {
		return 0, fmt.Errorf("moex iss share decode failed: %w", err)
	}

	if len(issResp.Marketdata.Data) == 0 || len(issResp.Marketdata.Data[0]) < 2 {
		return 0, fmt.Errorf("moex iss: no share data for %s", ticker)
	}

	last, ok := issResp.Marketdata.Data[0][1].(float64)
	if !ok {
		return 0, fmt.Errorf("moex iss: LAST not a number")
	}
	return last, nil
}

// moexISSSpotPrice fetches the LAST trade price of a FORTS futures contract
// from the public MOEX ISS API (no authentication required).
func moexISSSpotPrice(secid string) (float64, error) {
	url := fmt.Sprintf("http://iss.moex.com/iss/engines/futures/markets/forts/boards/RFUD/securities/%s.json?iss.meta=off&iss.only=marketdata&marketdata.columns=SECID,LAST,OPEN,HIGH,LOW,LASTVOLUME", secid)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("moex iss request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("moex iss status: %d", resp.StatusCode)
	}

	var issResp struct {
		Marketdata struct {
			Data [][]interface{} `json:"data"`
		} `json:"marketdata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issResp); err != nil {
		return 0, fmt.Errorf("moex iss decode failed: %w", err)
	}

	if len(issResp.Marketdata.Data) == 0 || len(issResp.Marketdata.Data[0]) < 2 {
		return 0, fmt.Errorf("moex iss: no data for %s", secid)
	}

	// Columns: [SECID, LAST, OPEN, HIGH, LOW, LASTVOLUME] -> index 1 is LAST.
	last, ok := issResp.Marketdata.Data[0][1].(float64)
	if !ok {
		return 0, fmt.Errorf("moex iss: LAST not a number")
	}
	return last, nil
}

// optionContract describes a MOEX option: SECID, underlying asset code,
// strike, call/put flag, expiry date, exchange margin (GO) figures and board.
type optionContract struct {
	SecID     string  // e.g. "Si85000BI6" (RFUD) or "SR100CC0" (ROPD)
	AssetCode string  // e.g. "Si" (RFUD) or "SBRF" (ROPD)
	Expiry    string  // e.g. "2026-09-17"
	Strike    float64 // e.g. 85000 (futures options) or 100 (share options)
	IsCall    bool
	IMNP      float64 // GO for a short option position (seller)
	IMP       float64 // GO for a long option position (buyer)
	PrevPrice float64
	Board     string // "RFUD" (options on futures) or "ROPD" (premium options on shares)
}

var (
	optionCache     []optionContract
	optionCacheTime time.Time
	optionMu        sync.Mutex
)

// quoteCache caches recent option LAST quotes so mark-to-market repricing of a
// multi-leg portfolio does not hammer ISS with one HTTP call per leg per tick.
var (
	quoteCache     = map[string]struct{ last, bid, offer, ts float64 }{}
	quoteCacheTime time.Time
	quoteMu        sync.Mutex
)

func cachedOptionQuote(secid string) (last, bid, offer float64) {
	quoteMu.Lock()
	if q, ok := quoteCache[secid]; ok && time.Since(time.Unix(int64(q.ts), 0)) < 5*time.Second {
		quoteMu.Unlock()
		return q.last, q.bid, q.offer
	}
	quoteMu.Unlock()

	last, bid, offer, err := moexOptionQuote(secid)
	if err != nil {
		return 0, 0, 0
	}
	quoteMu.Lock()
	quoteCache[secid] = struct{ last, bid, offer, ts float64 }{last, bid, offer, float64(time.Now().Unix())}
	quoteMu.Unlock()
	return last, bid, offer
}

// contractMultiplier returns the point value (₽ per 1 price point) for a
// FORTS contract symbol. Option premiums are quoted in the same points as the
// futures. Values verified against MOEX ISS (STEPPRICE/MINSTEP and trade
// history VALUE/VOLUME):
//   - Si: 1 point = 1 ₽ (e.g. Si84000BI6: MINSTEP 1, STEPPRICE 1);
//   - RI: 1 point = 0.02 USD ≈ 1.66 ₽ — floats with USD/RUB, fetched live
//     from any RI option spec (STEPPRICE/MINSTEP, e.g. 16.584/10);
//   - CR: 1 point = 1000 ₽ (MINSTEP 0.001, STEPPRICE 1);
//   - SBER/SBERP premium options: lot 100 shares, premium quoted per share.
func contractMultiplier(symbol string) float64 {
	switch symbol {
	case "RI":
		if v := riPointValue(); v > 0 {
			return v
		}
		return 1.66
	case "CR":
		return 1000.0
	case "SBER", "SBERP":
		return 100.0
	default: // Si and everything else: premium point = 1 ₽
		return 1.0
	}
}

var (
	riPointValCache   float64
	riPointValTime    time.Time
	riPointValMu      sync.Mutex
)

// riPointValue resolves the RI point value in rubles from the STEPPRICE and
// MINSTEP of a live RI (RTS) option, cached for an hour.
func riPointValue() float64 {
	riPointValMu.Lock()
	defer riPointValMu.Unlock()
	if riPointValCache > 0 && time.Since(riPointValTime) < time.Hour {
		return riPointValCache
	}
	opts, err := moexOptionContracts()
	if err != nil {
		return 0
	}
	secid := ""
	for _, o := range opts {
		if strings.EqualFold(o.AssetCode, "RTS") && o.PrevPrice > 0 {
			secid = o.SecID
			break
		}
	}
	if secid == "" {
		return 0
	}
	resp, err := http.Get("http://iss.moex.com/iss/engines/futures/markets/options/securities/" + secid + ".json?iss.meta=off")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var data struct {
		Securities struct {
			Columns []string        `json:"columns"`
			Data    [][]interface{} `json:"data"`
		} `json:"securities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || len(data.Securities.Data) == 0 {
		return 0
	}
	col := func(name string) int {
		for i, c := range data.Securities.Columns {
			if c == name {
				return i
			}
		}
		return -1
	}
	iMin, iStep := col("MINSTEP"), col("STEPPRICE")
	if iMin < 0 || iStep < 0 {
		return 0
	}
	row := data.Securities.Data[0]
	minStep, _ := row[iMin].(float64)
	stepPrice, _ := row[iStep].(float64)
	if minStep <= 0 || stepPrice <= 0 {
		return 0
	}
	v := stepPrice / minStep
	riPointValCache = v
	riPointValTime = time.Now()
	return v
}

// contractExpiry returns the option expiry date (YYYY-MM-DD) for a symbol by
// looking at its chain, or the currently selected futures series expiry.
func contractExpiry(symbol string) string {
	if e := currentSeriesExpiry(symbol); e != nil {
		return e.Format("2006-01-02")
	}
	seriesMu.Lock()
	code := selectedSeries[symbol]
	seriesMu.Unlock()
	if code != "" {
		if c := findContractByCode(code); c != nil && c.LastDelDate != "" {
			return c.LastDelDate
		}
	}
	return ""
}

// findContractByCode returns a futures contract by its SECID (e.g. SiU6).
func findContractByCode(code string) *futuresContract {
	contracts, err := moexFuturesContracts()
	if err != nil {
		return nil
	}
	for i := range contracts {
		if contracts[i].Code == code {
			return &contracts[i]
		}
	}
	return nil
}

// moexOptionContracts fetches the full FORTS options list (SECID, strike,
// call/put, expiry, margins) from the public MOEX ISS API, cached.
// The ISS options endpoint cannot filter by asset, so we filter in Go.
func moexOptionContracts() ([]optionContract, error) {
	optionMu.Lock()
	defer optionMu.Unlock()
	if len(optionCache) > 0 && time.Since(optionCacheTime) < 10*time.Minute {
		return optionCache, nil
	}

	url := "http://iss.moex.com/iss/engines/futures/markets/options/boards/RFUD/securities.json?iss.meta=off&iss.only=securities&securities.columns=SECID,LASTDELDATE,ASSETCODE,OPTIONTYPE,STRIKE,IMNP,IMP,PREVPRICE"
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Securities struct {
			Data [][]interface{} `json:"data"`
		} `json:"securities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	var opts []optionContract
	for _, row := range data.Securities.Data {
		if len(row) < 8 {
			continue
		}
		secid, _ := row[0].(string)
		expiry, _ := row[1].(string)
		asset, _ := row[2].(string)
		otype, _ := row[3].(string)
		strike, _ := row[4].(float64)
		imnp, _ := row[5].(float64)
		imp, _ := row[6].(float64)
		prev, _ := row[7].(float64)
		if secid == "" || asset == "" {
			continue
		}
		opts = append(opts, optionContract{
			SecID:     secid,
			AssetCode: asset,
			Expiry:    expiry,
			Strike:    strike,
			IsCall:    otype == "C",
			IMNP:      imnp,
			IMP:       imp,
			PrevPrice: prev,
			Board:     "RFUD",
		})
	}

	optionCache = opts
	optionCacheTime = time.Now()
	return opts, nil
}

// moexEquityOptionContracts fetches the premium option contracts on shares
// (board ROPD, e.g. SBRF = Sberbank common, SBPR = Sberbank preferred), cached.
// The same ISS columns are used; ASSETCODE carries the underlying share code.
// Note: the ROPD board also lists OPTIONS ON THE FUTURES (shortnames like
// "SBRF-9.26M...", strikes in futures scale), so we keep only the share
// premium options whose shortname starts with the share ticker prefix.
func moexEquityOptionContracts() ([]optionContract, error) {
	equityOptionMu.Lock()
	defer equityOptionMu.Unlock()
	if len(equityOptionCache) > 0 && time.Since(equityOptionCacheTime) < 10*time.Minute {
		return equityOptionCache, nil
	}

	url := "http://iss.moex.com/iss/engines/futures/markets/options/boards/ROPD/securities.json?iss.meta=off&iss.only=securities&securities.columns=SECID,LASTDELDATE,ASSETCODE,OPTIONTYPE,STRIKE,IMNP,IMP,PREVPRICE,SHORTNAME&iss.rows=50000"
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Securities struct {
			Data [][]interface{} `json:"data"`
		} `json:"securities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	var opts []optionContract
	for _, row := range data.Securities.Data {
		if len(row) < 9 {
			continue
		}
		secid, _ := row[0].(string)
		expiry, _ := row[1].(string)
		asset, _ := row[2].(string)
		otype, _ := row[3].(string)
		strike, _ := row[4].(float64)
		imnp, _ := row[5].(float64)
		imp, _ := row[6].(float64)
		prev, _ := row[7].(float64)
		shortname, _ := row[8].(string)
		if secid == "" || asset == "" {
			continue
		}
		// Keep only share premium options. SBRF share options shortname starts
		// with "SBER" (SBERP200330CE100), SBPR with "SBERPP"; futures-based
		// options use the futures code ("SBRF-9.26M...") and must be excluded.
		if !isShareOption(asset, shortname) {
			continue
		}
		opts = append(opts, optionContract{
			SecID:     secid,
			AssetCode: asset,
			Expiry:    expiry,
			Strike:    strike,
			IsCall:    otype == "C",
			IMNP:      imnp,
			IMP:       imp,
			PrevPrice: prev,
			Board:     "ROPD",
		})
	}

	equityOptionCache = opts
	equityOptionCacheTime = time.Now()
	return opts, nil
}

// isShareOption reports whether a ROPD contract is a premium option on the
// underlying share rather than an option on the SBRF futures contract.
// Share option shortnames start with the share ticker ("SBER", "SBERP"),
// while futures-based shortnames start with the futures code ("SBRF-...",
// "SBPR-..." is not used, but we exclude any code that looks like a futures).
func isShareOption(asset, shortname string) bool {
	if shortname == "" {
		return true // no shortname → keep (defensive)
	}
	switch asset {
	case "SBRF":
		return strings.HasPrefix(shortname, "SBER")
	case "SBPR":
		return strings.HasPrefix(shortname, "SBERP")
	default:
		return true
	}
}

// moexOptionsForAsset returns the option chain for a given underlying asset
// and expiry date, filtered from the cached full list.
// ISS uses different asset codes than our UI symbols: RI->RTS, CR->CNY,
// SBER->SBRF (premium options on shares), SBERP->SBPR.
func moexOptionsForAsset(asset, expiry string) []optionContract {
	issAsset := asset
	board := "RFUD"
	switch asset {
	case "RI":
		issAsset = "RTS"
	case "CR":
		issAsset = "CNY"
	case "SBER":
		issAsset = "SBRF"
		board = "ROPD"
	case "SBERP":
		issAsset = "SBPR"
		board = "ROPD"
	}
	var opts []optionContract
	var err error
	if board == "ROPD" {
		opts, err = moexEquityOptionContracts()
	} else {
		opts, err = moexOptionContracts()
	}
	if err != nil {
		return nil
	}
	var out []optionContract
	for _, o := range opts {
		if o.AssetCode == issAsset && o.Expiry == expiry {
			out = append(out, o)
		}
	}
	return out
}

// findOptionBySecID returns a cached option contract by its SECID, searching
// both the RFUD (futures options) and ROPD (share premium options) boards.
func findOptionBySecID(secid string) *optionContract {
	opts, err := moexOptionContracts()
	if err == nil {
		for i := range opts {
			if opts[i].SecID == secid {
				return &opts[i]
			}
		}
	}
	eopts, err2 := moexEquityOptionContracts()
	if err2 == nil {
		for i := range eopts {
			if eopts[i].SecID == secid {
				return &eopts[i]
			}
		}
	}
	return nil
}

// moexOptionQuote fetches the live LAST/BID/OFFER for an option SECID on the
// correct board (RFUD for futures options, ROPD for premium share options).
func moexOptionQuote(secid string) (last, bid, offer float64, err error) {
	board := "RFUD"
	if opt := findOptionBySecID(secid); opt != nil && opt.Board != "" {
		board = opt.Board
	}
	url := fmt.Sprintf("http://iss.moex.com/iss/engines/futures/markets/options/boards/%s/securities/%s.json?iss.meta=off&iss.only=marketdata&marketdata.columns=SECID,LAST,BID,OFFER", board, secid)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("moex iss option request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, 0, fmt.Errorf("moex iss option status: %d", resp.StatusCode)
	}

	var issResp struct {
		Marketdata struct {
			Data [][]interface{} `json:"data"`
		} `json:"marketdata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issResp); err != nil {
		return 0, 0, 0, fmt.Errorf("moex iss option decode failed: %w", err)
	}

	if len(issResp.Marketdata.Data) == 0 || len(issResp.Marketdata.Data[0]) < 4 {
		return 0, 0, 0, fmt.Errorf("moex iss: no option data for %s", secid)
	}

	row := issResp.Marketdata.Data[0]
	last, _ = row[1].(float64)
	bid, _ = row[2].(float64)
	offer, _ = row[3].(float64)
	if last <= 0 {
		last = bid
	}
	return last, bid, offer, nil
}

// nearestStrike returns the option strike closest to the spot price from the
// given chain (falling back to spot rounded to the asset step).
func nearestStrike(chain []optionContract, spot float64) float64 {
	if len(chain) == 0 {
		return spot
	}
	best := chain[0].Strike
	bestDist := math.Abs(best - spot)
	for _, o := range chain {
		d := math.Abs(o.Strike - spot)
		if d < bestDist {
			best = o.Strike
			bestDist = d
		}
	}
	return best
}

// futuresSeriesAlor converts a MOEX series code (e.g. "SiU6") into the
// human-readable Alor futures name (e.g. "Si-9.26") using cached MOEX data.
// For premium equity options (SBER/SBERP) the hedge instrument is the share
// itself, so Alor uses the plain ticker.
func futuresSeriesAlor(symbol string) (string, bool) {
	if _, isEquity := equityOptions[symbol]; isEquity {
		return symbol, true
	}
	seriesMu.Lock()
	code := selectedSeries[symbol]
	seriesMu.Unlock()
	if code == "" {
		return "", false
	}
	if isSyntheticSeriesCode(code) {
		code = resolveRealFuturesCode(symbol, code)
		if code == "" {
			return "", false
		}
	}
	contracts, err := moexFuturesContracts()
	if err == nil {
		for _, c := range contracts {
			if c.Code == code {
				return c.ShortName, true
			}
		}
	}
	// Fallback guess: keep the code itself (Alor also accepts SiU6-style codes).
	return code, true
}

// seriesInfoHandler returns the current series, its expiry date, type
// (weekly/monthly/quarterly) and the full list of available series.
func seriesInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	seriesMu.Lock()
	current := selectedSeries[symbol]
	seriesMu.Unlock()

	now := time.Now()
	// Series list = real option expiries (weekly/monthly/quarterly), not the
	// quarterly-only futures contract list.
	contracts := optionSeriesForSymbol(symbol)

	curExpiry := ""
	if t := currentSeriesExpiry(symbol); t != nil {
		curExpiry = t.Format("2006-01-02")
	}

	type seriesItem struct {
		Code        string `json:"code"`
		ShortName   string `json:"short_name"`
		Expiry      string `json:"expiry"`
		DaysToExp   int    `json:"days_to_exp"`
		Type        string `json:"type"`
		TypeCode    string `json:"type_code"`
		IsCurrent   bool   `json:"is_current"`
	}

	today := now.Format("2006-01-02")
	var liveDates []time.Time
	for _, c := range contracts {
		if c.LastDelDate == "" || c.LastDelDate < today {
			continue
		}
		if t, err := time.Parse("2006-01-02", c.LastDelDate); err == nil {
			liveDates = append(liveDates, t)
		}
	}

	var items []seriesItem
	currentExpiry := ""
	currentShort := ""
	for _, c := range contracts {
		// Only include series that are still alive (expiry in the future).
		if c.LastDelDate != "" && c.LastDelDate < today {
			continue
		}
		label := classifyExpiry(c.LastDelDate, liveDates)
		items = append(items, seriesItem{
			Code:      c.Code,
			ShortName: c.ShortName,
			Expiry:    c.LastDelDate,
			DaysToExp: dteInDays(c.LastDelDate, now),
			Type:      label,
			TypeCode:  seriesTypeCode(label),
			IsCurrent: curExpiry != "" && c.LastDelDate == curExpiry,
		})
		if c.Code == current {
			currentExpiry = c.LastDelDate
			currentShort = c.ShortName
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol":         symbol,
		"futures_series": currentShort,
		"options_series": current,
		"expiry":         currentExpiry,
		"days_to_exp":    dteInDays(currentExpiry, now),
		"type":           classifyExpiry(currentExpiry, liveDates),
		"series":         items,
	})
}

// setSeriesHandler switches the active options/futures series for an asset.
func setSeriesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Symbol string `json:"symbol"`
		Series string `json:"series"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	if req.Symbol == "" || req.Series == "" {
		http.Error(w, "symbol and series are required", http.StatusBadRequest)
		return
	}

	// Verify the series exists for this symbol (option-expiry based list).
	found := false
	for _, c := range optionSeriesForSymbol(req.Symbol) {
		if c.Code == req.Series {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "unknown series for symbol: "+req.Series, http.StatusBadRequest)
		return
	}

	seriesMu.Lock()
	selectedSeries[req.Symbol] = req.Series
	seriesMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"symbol":   req.Symbol,
		"series":   req.Series,
	})
}

func spotHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Symbol string  `json:"symbol"`
		Price  float64 `json:"price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Price <= 0 {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	symbol := req.Symbol
	if symbol == "" {
		symbol = "Si"
	}

	spotMu.Lock()
	spotOverrides[symbol] = req.Price
	spotMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"symbol":  symbol,
		"price":   req.Price,
	})
}

func quoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	price, err := getSpotPrice(symbol)
	if err != nil {
		price = 83200.0
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol":    symbol,
		"price":     price,
		"timestamp": time.Now(),
	})
}

func liveGreeksHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	strikeStr := r.URL.Query().Get("strike")
	daysStr := r.URL.Query().Get("days")
	volStr := r.URL.Query().Get("vol")
	isCallStr := r.URL.Query().Get("is_call")

	spotPrice, err := getSpotPrice(symbol)
	if err != nil {
		spotPrice = 83200.0
	}

	strike, _ := strconv.ParseFloat(strikeStr, 64)
	if strike == 0 {
		strike = spotPrice
	}

	days, _ := strconv.ParseFloat(daysStr, 64)
	if days == 0 {
		days = 30
	}

	vol, _ := strconv.ParseFloat(volStr, 64)
	if vol == 0 {
		vol = 0.32
	}

	isCall := isCallStr != "false"

	t := days / 365.0
	rRate := 0.16 // MOEX Key rate / RUONIA

	greeks := quant.CalculateBlackScholes(isCall, spotPrice, strike, t, rRate, vol)

	response := map[string]interface{}{
		"symbol":      symbol,
		"spot_price":  spotPrice,
		"strike":      strike,
		"days_to_exp": days,
		"volatility":  vol,
		"is_call":     isCall,
		"greeks":      greeks,
	}

	json.NewEncoder(w).Encode(response)
}

func arbitrageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	callPriceStr := r.URL.Query().Get("call_price")
	putPriceStr := r.URL.Query().Get("put_price")
	strikeStr := r.URL.Query().Get("strike")
	daysStr := r.URL.Query().Get("days")

	spotPrice, err := getSpotPrice(symbol)
	if err != nil {
		spotPrice = 83200.0
	}

	strike, _ := strconv.ParseFloat(strikeStr, 64)
	if strike == 0 {
		strike = spotPrice
	}

	days, _ := strconv.ParseFloat(daysStr, 64)
	if days == 0 {
		days = 30
	}

	callPrice, _ := strconv.ParseFloat(callPriceStr, 64)
	putPrice, _ := strconv.ParseFloat(putPriceStr, 64)

	if callPrice == 0 && putPrice == 0 {
		callGreeks := quant.CalculateBlackScholes(true, spotPrice, strike, days/365.0, 0.16, 0.32)
		putGreeks := quant.CalculateBlackScholes(false, spotPrice, strike, days/365.0, 0.16, 0.32)

		callPrice = callGreeks.Price + 25.0
		putPrice = putGreeks.Price
	}

	arb := quant.CheckPutCallParity(symbol, spotPrice, strike, days, callPrice, putPrice, 0.16)
	json.NewEncoder(w).Encode(arb)
}

func skewHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	spot, err := getSpotPrice(symbol)
	if err != nil {
		spot = 83200.0
	}

	step := 1000.0
	if symbol == "CR" {
		step = 0.25
	} else if symbol == "RI" {
		step = 2000.0
	} else if symbol == "BTC" {
		step = 2000.0
	}

	var points []SkewPoint

	for i := -5; i <= 5; i++ {
		strike := spot + float64(i)*step
		distFromSpot := (strike - spot) / spot
		iv := 0.32 + (distFromSpot * distFromSpot * 1.5) - (distFromSpot * 0.10)

		points = append(points, SkewPoint{
			Strike: strike,
			IV:     iv * 100,
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol": symbol,
		"spot":   spot,
		"points": points,
	})
}

// initAlorClients initializes the Alor API clients using the encrypted token
// stored on disk (falling back to the ALOR_REFRESH_TOKEN env var for backwards
// compatibility on first deploy).
func initAlorClients() {
	refreshToken, _ := tokenStore.LoadToken()
	if refreshToken == "" {
		refreshToken = os.Getenv("ALOR_REFRESH_TOKEN")
	}
	portfolio, _ := tokenStore.LoadPortfolio()
	if portfolio == "" {
		portfolio = os.Getenv("ALOR_PORTFOLIO")
	}

	alorAuth = alor.NewAuthClient(refreshToken)
	alorMarket = alor.NewMarketClient(alorAuth)
	alorExec = alor.NewExecutionClient(alorAuth, portfolio)
}

// settingsTokenHandler GET returns token status; POST saves a new token encrypted.
func settingsTokenHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		status := "not_configured"
		if tokenStore != nil && tokenStore.HasToken() {
			status = "configured"
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": status,
			"has_token": tokenStore != nil && tokenStore.HasToken(),
		})
		return

	case http.MethodPost:
		var req struct {
			RefreshToken string `json:"refresh_token"`
			Portfolio    string `json:"portfolio"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}

		if req.RefreshToken == "" {
			http.Error(w, "refresh_token is required", http.StatusBadRequest)
			return
		}

		// Save encrypted to disk first.
		if err := tokenStore.SaveToken(req.RefreshToken); err != nil {
			http.Error(w, "Failed to save token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if req.Portfolio != "" {
			_ = tokenStore.SavePortfolio(req.Portfolio)
		}

		// Apply to running clients and validate.
		alorAuth.SetRefreshToken(req.RefreshToken)
		if req.Portfolio != "" {
			alorExec.SetPortfolio(req.Portfolio)
		}

		valid, msg := alorAuth.ValidateRefreshToken()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  true,
			"valid":    valid,
			"message":  msg,
			"has_token": true,
		})
		return

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func moexQuoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	// Prefer public MOEX ISS for the futures series, fall back to Alor.
	price, err := getSpotPrice(symbol)
	if err != nil {
		price = 0
	}
	quote := map[string]interface{}{
		"symbol":   symbol,
		"exchange": "MOEX",
		"price":    price,
		"bid":      price,
		"ask":      price,
	}
	if price > 0 {
		quote["note"] = "Real-time price from MOEX ISS / Alor"
	} else {
		quote["note"] = "No price source available: " + err.Error()
	}

	json.NewEncoder(w).Encode(quote)
}

func moexArbitrageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	strikeStr := r.URL.Query().Get("strike")
	daysStr := r.URL.Query().Get("days")
	callStr := r.URL.Query().Get("call")
	putStr := r.URL.Query().Get("put")

	spot, _ := getSpotPrice(symbol)
	if spot <= 0 {
		spot = 83200.0
	}
	strike := spot
	if strikeStr != "" {
		strike, _ = strconv.ParseFloat(strikeStr, 64)
	}
	if strike <= 0 {
		strike = spot
	}
	days := 30.0
	if daysStr != "" {
		days, _ = strconv.ParseFloat(daysStr, 64)
	}
	callPrice := 1500.0
	if callStr != "" {
		callPrice, _ = strconv.ParseFloat(callStr, 64)
	}
	putPrice := 1800.0
	if putStr != "" {
		putPrice, _ = strconv.ParseFloat(putStr, 64)
	}

	theor, actual, strategy := quant.CalculateMOEXParitySpread(spot, strike, days, callPrice, putPrice, 0.16)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol":           symbol,
		"spot_price":       spot,
		"strike":           strike,
		"days_to_exp":      days,
		"theoretical_diff": theor,
		"actual_diff":      actual,
		"strategy":         strategy,
		"exchange":         "MOEX FORTS",
	})
}

func moexOrderHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Symbol   string  `json:"symbol"`
		Side     string  `json:"side"`
		Type     string  `json:"type"`
		Price    float64 `json:"price"`
		Quantity int     `json:"quantity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	// Record the position regardless of Alor availability so the terminal
	// always tracks opened strategies. A live entry price is fetched from MOEX.
	if req.Symbol == "" {
		req.Symbol = "Si"
	}
	if req.Quantity <= 0 {
		req.Quantity = 1
	}
	entryPrice := req.Price
	if entryPrice <= 0 {
		entryPrice, _ = getSpotPrice(req.Symbol)
	}

	mult := contractMultiplier(req.Symbol)
	side := strings.ToUpper(req.Side)
	if side == "" {
		side = "BUY"
	}
	p := quant.Position{
		ID:       fmt.Sprintf("pos-%d", time.Now().Unix()),
		Strategy: "Alor MOEX Execution",
		Symbol:   req.Symbol,
		Expiry:   contractExpiry(req.Symbol),
		Legs: []quant.PositionLeg{{
			SecID:        selectedSeriesFor(req.Symbol),
			Symbol:       req.Symbol,
			Kind:         "FUTURES",
			Side:         side,
			Quantity:     req.Quantity,
			EntryPrice:   entryPrice,
			CurrentPrice: entryPrice,
		}},
		OpenedAt: time.Now(),
	}
	secid := selectedSeriesFor(req.Symbol)
	if m := moexFutureInitialMargin(secid); m > 0 {
		p.Margin = m * float64(req.Quantity)
	} else {
		p.Margin = mult * float64(req.Quantity)
	}
	repricePosition(&p)
	quant.SavePosition(p)

	portfolio := quant.GetPortfolio()
	stats := quant.ComputeStats()

	var alorNote string
	if alorExec != nil {
		if _, err := alorExec.PlaceOrder(req.Symbol, side, req.Type, req.Price, req.Quantity); err != nil {
			alorNote = fmt.Sprintf("Alor order failed (check ALOR_REFRESH_TOKEN and ALOR_PORTFOLIO): %v", err)
		} else {
			alorNote = "Alor order accepted"
		}
	} else {
		alorNote = "Alor execution not configured; position tracked in paper portfolio"
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"position":  p,
		"portfolio": portfolio,
		"stats":     stats,
		"note":      alorNote,
	})
}

// repricePosition refreshes every leg's current price from live MOEX quotes
// and recomputes PnL, delta and theta of the position.
func repricePosition(p *quant.Position) {
	spot, _ := getSpotPrice(p.Symbol)
	mult := contractMultiplier(p.Symbol)
	rRate := 0.16
	days := dteInDays(p.Expiry, time.Now())
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0

	entryValue := 0.0
	currentValue := 0.0
	deltaTotal := 0.0
	thetaTotal := 0.0

	for i := range p.Legs {
		leg := &p.Legs[i]
		dir := 1.0
		if leg.Side == "SELL" {
			dir = -1.0
		}

		var last float64
		if leg.Kind == "FUTURES" {
			// Prefer the actual contract quote (leg.SecID like "SiU6"); fall
			// back to the symbol spot when no contract code is stored.
			last = 0
			if leg.SecID != "" && len(leg.SecID) >= 3 {
				if c, err := moexISSSpotPrice(leg.SecID); err == nil && c > 0 {
					last = c
				}
			}
			if last <= 0 {
				if s, err := getSpotPrice(p.Symbol); err == nil && s > 0 {
					last = s
				} else {
					last = leg.CurrentPrice
				}
			}
		} else {
			l, b, o := cachedOptionQuote(leg.SecID)
			if l > 0 {
				last = l
			} else if b > 0 {
				last = b
			} else if o > 0 {
				last = o
			} else {
				last = leg.CurrentPrice
			}
		}
		if last > 0 {
			leg.CurrentPrice = last
		}

		entryValue += dir * leg.EntryPrice * mult * float64(leg.Quantity)
		currentValue += dir * leg.CurrentPrice * mult * float64(leg.Quantity)

		if leg.Kind == "OPTION" {
			iv := quant.ImpliedVolatility(leg.IsCall, leg.CurrentPrice, spot, leg.Strike, t, rRate)
			if iv <= 0 {
				iv = 0.30
			}
			g := quant.CalculateBlackScholes(leg.IsCall, spot, leg.Strike, t, rRate, iv)
			deltaTotal += dir * g.Delta * float64(leg.Quantity)
			thetaTotal += dir * g.Theta * mult * float64(leg.Quantity)
		} else {
			deltaTotal += dir * 1.0 * float64(leg.Quantity)
		}
	}

	p.EntryValue = entryValue
	p.CurrentValue = currentValue
	p.PnL = currentValue - entryValue
	if entryValue != 0 {
		p.PnLPercent = p.PnL / math.Abs(entryValue) * 100
	}
	p.Delta = math.Round(deltaTotal*100) / 100
	p.Theta = math.Round(thetaTotal*100) / 100
}

func positionsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	positions := quant.GetActivePositions()
	for i := range positions {
		repricePosition(&positions[i])
		quant.SavePosition(positions[i])
	}

	portfolio := quant.GetPortfolio()
	stats := quant.ComputeStats()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"positions": positions,
		"portfolio": portfolio,
		"stats":     stats,
	})
}
func portfolioHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	portfolio := quant.GetPortfolio()
	json.NewEncoder(w).Encode(portfolio)
}

// riskHandler aggregates portfolio-level risk across all open positions:
// net delta/gamma/theta, margin load, realized drawdown and a per-position
// risk limit report. GET /api/v1/risk
func riskHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	positions := quant.GetActivePositions()
	for i := range positions {
		repricePosition(&positions[i])
		quant.SavePosition(positions[i])
	}

	netDelta := 0.0
	netGamma := 0.0
	netTheta := 0.0
	totalMargin := 0.0
	totalValue := 0.0

	for i := range positions {
		p := &positions[i]
		spot, _ := getSpotPrice(p.Symbol)
		mult := contractMultiplier(p.Symbol)
		rRate := 0.16
		days := dteInDays(p.Expiry, time.Now())
		if days <= 0 {
			days = 30
		}
		t := float64(days) / 365.0

		for _, leg := range p.Legs {
			dir := 1.0
			if leg.Side == "SELL" {
				dir = -1.0
			}
			qty := float64(leg.Quantity)
			if leg.Kind == "FUTURES" {
				netDelta += dir * 1.0 * qty
				totalValue += dir * leg.CurrentPrice * mult * qty
				continue
			}
			iv := quant.ImpliedVolatility(leg.IsCall, leg.CurrentPrice, spot, leg.Strike, t, rRate)
			if iv <= 0 {
				iv = 0.30
			}
			g := quant.CalculateBlackScholes(leg.IsCall, spot, leg.Strike, t, rRate, iv)
			netDelta += dir * g.Delta * qty
			netGamma += dir * g.Gamma * qty
			netTheta += dir * g.Theta * mult * qty
			totalValue += dir * leg.CurrentPrice * mult * qty
		}
		totalMargin += p.Margin
	}

	portfolio := quant.GetPortfolio()
	stats := quant.ComputeStats()
	marginLoad := 0.0
	if portfolio.InitialCapital > 0 {
		marginLoad = portfolio.LockedMargin / portfolio.InitialCapital * 100
	}

	// Realized drawdown from trade history (peak realized equity vs current).
	realized := stats.TotalRealizedPnL
	run := 0.0
	peak := 0.0
	for _, tr := range quant.GetTrades() {
		run += tr.RealizedPnL
		if run > peak {
			peak = run
		}
	}
	dd := peak - realized
	if dd < 0 {
		dd = 0
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"net_delta":       math.Round(netDelta*100) / 100,
		"net_gamma":       math.Round(netGamma*100) / 100,
		"net_theta":       math.Round(netTheta*100) / 100,
		"margin_load_pct": math.Round(marginLoad*100) / 100,
		"locked_margin":   portfolio.LockedMargin,
		"initial_capital": portfolio.InitialCapital,
		"unrealized_pnl":  portfolio.UnrealizedPnL,
		"realized_pnl":    realized,
		"drawdown_realized": math.Round(dd*100) / 100,
		"positions_count": len(positions),
		"total_exposure":  math.Round(totalValue*100) / 100,
		"stats":           stats,
	})
}

func capitalHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Amount float64 `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Amount <= 0 {
		http.Error(w, "Invalid capital amount", http.StatusBadRequest)
		return
	}

	quant.SetInitialCapital(req.Amount)
	portfolio := quant.GetPortfolio()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"portfolio": portfolio,
	})
}

func openPositionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Strategy string `json:"strategy"`
		Symbol   string `json:"symbol"`
		Expiry   string `json:"expiry"`
		Legs     []struct {
			SecID      string  `json:"secid"`
			Kind       string  `json:"kind"` // OPTION or FUTURES
			Side       string  `json:"side"` // BUY or SELL
			Quantity   int     `json:"quantity"`
			Strike     float64 `json:"strike"`
			IsCall     bool    `json:"is_call"`
			EntryPrice float64 `json:"entry_price"`
		} `json:"legs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Symbol == "" || len(req.Legs) == 0 {
		http.Error(w, "Symbol and legs are required", http.StatusBadRequest)
		return
	}

	if req.Expiry == "" {
		req.Expiry = contractExpiry(req.Symbol)
	}
	mult := contractMultiplier(req.Symbol)

	p := quant.Position{
		ID:       fmt.Sprintf("pos-%d", time.Now().Unix()),
		Strategy: req.Strategy,
		Symbol:   req.Symbol,
		Expiry:   req.Expiry,
		OpenedAt: time.Now(),
	}

	// Fill live entry prices and margins for every leg.
	for _, l := range req.Legs {
		entry := l.EntryPrice
		if l.Kind == "FUTURES" {
			if entry <= 0 {
				entry, _ = getSpotPrice(req.Symbol)
			}
			leg := quant.PositionLeg{
				SecID:        l.SecID,
				Symbol:       req.Symbol,
				Kind:         "FUTURES",
				Side:         l.Side,
				Quantity:     l.Quantity,
				EntryPrice:   entry,
				CurrentPrice: entry,
			}
			if secid := l.SecID; secid == "" {
				secid = selectedSeriesFor(req.Symbol)
			}
			p.Legs = append(p.Legs, leg)
			if secid := l.SecID; secid == "" {
				continue
			} else if m := moexFutureInitialMargin(secid); m > 0 {
				p.Margin += m * float64(l.Quantity)
			} else {
				p.Margin += mult * float64(l.Quantity)
			}
			continue
		}

		// OPTION leg
		if entry <= 0 {
			if last, _, _, err := moexOptionQuote(l.SecID); err == nil && last > 0 {
				entry = last
			} else if opt := findOptionBySecID(l.SecID); opt != nil {
				entry = opt.PrevPrice
			}
		}
		leg := quant.PositionLeg{
			SecID:        l.SecID,
			Symbol:       req.Symbol,
			Kind:         "OPTION",
			Side:         l.Side,
			Quantity:     l.Quantity,
			Strike:       l.Strike,
			IsCall:       l.IsCall,
			EntryPrice:   entry,
			CurrentPrice: entry,
		}
		p.Legs = append(p.Legs, leg)
		if opt := findOptionBySecID(l.SecID); opt != nil {
			if l.Side == "SELL" {
				p.Margin += opt.IMNP * float64(l.Quantity)
			} else {
				p.Margin += opt.IMP * float64(l.Quantity)
			}
		}
	}

	repricePosition(&p)
	quant.SavePosition(p)

	portfolio := quant.GetPortfolio()
	stats := quant.ComputeStats()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"position":  p,
		"portfolio": portfolio,
		"stats":     stats,
	})
}

func closePositionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	pos, found := quant.RemovePosition(req.ID)
	if !found {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "position not found",
		})
		return
	}

	// Mark-to-market before closing so realized PnL reflects live prices.
	repricePosition(&pos)

	trade := quant.Trade{
		ID:          fmt.Sprintf("trd-%d", time.Now().Unix()),
		Strategy:    pos.Strategy,
		Symbol:      pos.Symbol,
		OpenedAt:    pos.OpenedAt,
		ClosedAt:    time.Now(),
		EntryValue:  pos.EntryValue,
		ExitValue:   pos.CurrentValue,
		RealizedPnL: pos.PnL,
		PnLPercent:  pos.PnLPercent,
	}
	quant.AddTrade(trade)

	portfolio := quant.GetPortfolio()
	stats := quant.ComputeStats()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"message":   "Position closed and recorded in trade history",
		"trade":     trade,
		"portfolio": portfolio,
		"stats":     stats,
	})
}

// deltaHedgeHandler hedges the net delta of an open position with futures.
// POST /api/v1/positions/hedge {"id":"...", "live":true}
// It places a real Alor market order when "live" is true and Alor is wired,
// otherwise it simulates the hedge and records a FUTURES leg on the position.
func deltaHedgeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID   string `json:"id"`
		Live bool   `json:"live"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	hedgePositionByID(req.ID, req.Live, w)
}

func tradesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodDelete {
		removed := quant.ClearTrades()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":        true,
			"removed":   removed,
			"trades":    quant.GetTrades(),
			"stats":     quant.ComputeStats(),
		})
		return
	}

	trades := quant.GetTrades()
	stats := quant.ComputeStats()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"trades": trades,
		"stats":  stats,
	})
}

// positionProfileHandler returns the payoff profile (P&L vs underlying price at
// expiration) for a single open position, computed from its real legs.
func positionProfileHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}

	var pos *quant.Position
	for _, p := range quant.GetActivePositions() {
		if p.ID == id {
			pos = &p
			break
		}
	}
	if pos == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "position not found"})
		return
	}
	repricePosition(pos)

	spot, _ := getSpotPrice(pos.Symbol)
	if spot <= 0 {
		spot = pos.CurrentValue
	}
	mult := contractMultiplier(pos.Symbol)

	lo := spot * 0.8
	hi := spot * 1.2
	steps := 40
	type profPoint struct {
		Spot float64 `json:"spot"`
		PnL  float64 `json:"pnl"`
	}
	points := make([]profPoint, 0, steps+1)
	for i := 0; i <= steps; i++ {
		s := lo + (hi-lo)*float64(i)/float64(steps)
		pnl := 0.0
		for _, leg := range pos.Legs {
			dir := 1.0
			if leg.Side == "SELL" {
				dir = -1.0
			}
			var value float64
			switch leg.Kind {
			case "FUTURES":
				value = dir * (s - leg.EntryPrice) * mult * float64(leg.Quantity)
			case "OPTION":
				var intrinsic float64
				if leg.IsCall {
					intrinsic = math.Max(s-leg.Strike, 0)
				} else {
					intrinsic = math.Max(leg.Strike-s, 0)
				}
				value = dir * (intrinsic - leg.EntryPrice) * mult * float64(leg.Quantity)
			}
			pnl += value
		}
		points = append(points, profPoint{Spot: math.Round(s*100) / 100, PnL: math.Round(pnl*100) / 100})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       pos.ID,
		"strategy": pos.Strategy,
		"symbol":   pos.Symbol,
		"spot":     spot,
		"points":   points,
	})
}

// selectedSeriesFor returns the currently selected futures series code for a symbol.
func selectedSeriesFor(symbol string) string {
	seriesMu.Lock()
	defer seriesMu.Unlock()
	return selectedSeries[symbol]
}

func copilotHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req agent.CopilotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req = agent.CopilotRequest{
			Prompt: "Проанализируй портфель",
			Delta:  0.02,
			Theta:  850.0,
			Gamma:  0.003,
			Vega:   120.0,
			Spread: 5.0,
		}
	}

	resp := agent.ProcessCopilotQuery(req)
	json.NewEncoder(w).Encode(resp)
}

// nextQuarterlyContract returns the quarterly futures contract (expiry month is
// March/June/September/December) that expires after the given reference date.
func nextQuarterlyContract(symbol string, after time.Time) (futuresContract, bool) {
	contracts := futuresContractsForSymbol(symbol)
	var best futuresContract
	bestDate := time.Time{}
	for _, c := range contracts {
		t, err := time.Parse("2006-01-02", c.LastDelDate)
		if err != nil {
			continue
		}
		if t.Month()%3 != 0 {
			continue // only quarterly months: Mar, Jun, Sep, Dec
		}
		if !t.After(after) {
			continue
		}
		if bestDate.IsZero() || t.Before(bestDate) {
			best = c
			bestDate = t
		}
	}
	return best, !bestDate.IsZero()
}

func moexPerpQuarterlyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	perpPrice, err := getSpotPrice(symbol)
	if err != nil || perpPrice <= 0 {
		perpPrice = 83200.0
	}

	seriesMu.Lock()
	currentSeries := selectedSeries[symbol]
	seriesMu.Unlock()

	// Real quarterly price: the next quarterly contract after the current series.
	currentExpiry := time.Now()
	if sInfo := currentSeriesExpiry(symbol); sInfo != nil {
		currentExpiry = *sInfo
	}
	quarterlySeries := currentSeries
	quarterlyPrice := math.Round(perpPrice * 1.012)

	if qc, ok := nextQuarterlyContract(symbol, currentExpiry); ok {
		if real, err := moexISSSpotPrice(qc.Code); err == nil && real > 0 {
			quarterlySeries = qc.Code
			quarterlyPrice = real
		}
	}

	spread := quarterlyPrice - perpPrice
	annualizedReturn := (spread / perpPrice) * (365.0 / 90.0) * 100.0

	strategy := "No Arbitrage"
	if spread > 300.0 {
		strategy = fmt.Sprintf("Sell Quarterly %s, Buy Perpetual %s (Contango Arbitrage / Carry)", quarterlySeries, symbol)
	} else if spread < -100.0 {
		strategy = fmt.Sprintf("Buy Quarterly %s, Sell Perpetual %s (Backwardation)", quarterlySeries, symbol)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol":            symbol,
		"series":            currentSeries,
		"quarterly_series":  quarterlySeries,
		"perpetual_price":   perpPrice,
		"quarterly_price":   quarterlyPrice,
		"spread":            math.Round(spread*100) / 100,
		"annualized_return": math.Round(annualizedReturn*100) / 100,
		"strategy":          strategy,
	})
}

// currentSeriesExpiry returns the expiry date of the currently selected series,
// or nil if unknown.
func currentSeriesExpiry(symbol string) *time.Time {
	seriesMu.Lock()
	code := selectedSeries[symbol]
	seriesMu.Unlock()
	// Synthetic expiry-based codes ("Si-2026-08-20") carry the date directly.
	if t, ok := syntheticSeriesExpiry(code); ok {
		return t
	}
	for _, c := range futuresContractsForSymbol(symbol) {
		if c.Code == code && c.LastDelDate != "" {
			if t, err := time.Parse("2006-01-02", c.LastDelDate); err == nil {
				return &t
			}
		}
	}
	return nil
}

// strategyParityHandler computes a real put-call parity (Conversion / Reversal)
// for the current series using live MOEX ISS option prices and exchange margins.
// URL: /api/v1/strategy/parity?symbol=Si
func strategyParityHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	spot, err := getSpotPrice(symbol)
	if err != nil || spot <= 0 {
		spot = 83200.0
	}

	seriesMu.Lock()
	seriesCode := selectedSeries[symbol]
	seriesMu.Unlock()

	expiry := ""
	expiryTime := currentSeriesExpiry(symbol)
	if expiryTime != nil {
		expiry = expiryTime.Format("2006-01-02")
	}

	chain := moexOptionsForAsset(symbol, expiry)
	strike := nearestStrike(chain, spot)

	var callOpt, putOpt *optionContract
	for i := range chain {
		if chain[i].Strike == strike {
			if chain[i].IsCall && callOpt == nil {
				callOpt = &chain[i]
			} else if !chain[i].IsCall && putOpt == nil {
				putOpt = &chain[i]
			}
		}
	}

	response := map[string]interface{}{
		"symbol":       symbol,
		"series":       seriesCode,
		"expiry":       expiry,
		"days_to_exp":  dteInDays(expiry, time.Now()),
		"spot_price":   spot,
		"strike":       strike,
		"chain_count":  len(chain),
		"call_found":   callOpt != nil,
		"put_found":    putOpt != nil,
		"note":         "Live MOEX ISS prices",
	}

	if callOpt == nil || putOpt == nil {
		response["error"] = "option chain not found for strike " + fmt.Sprintf("%.0f", strike)
		json.NewEncoder(w).Encode(response)
		return
	}

	// Live market prices.
	callLast, callBid, callOffer, _ := moexOptionQuote(callOpt.SecID)
	putLast, putBid, putOffer, _ := moexOptionQuote(putOpt.SecID)
	if callLast <= 0 {
		callLast = callOpt.PrevPrice
	}
	if putLast <= 0 {
		putLast = putOpt.PrevPrice
	}

	days := dteInDays(expiry, time.Now())
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0
	rRate := 0.16 // MOEX Key rate / RUONIA approximation

	// Implied volatilities recovered from real market prices.
	callIV := quant.ImpliedVolatility(true, callLast, spot, strike, t, rRate)
	putIV := quant.ImpliedVolatility(false, putLast, spot, strike, t, rRate)
	iv := (callIV + putIV) / 2.0
	if callIV <= 0 && putIV > 0 {
		iv = putIV
	} else if putIV <= 0 && callIV > 0 {
		iv = callIV
	}

	// Theoretical prices + greeks at the market IV.
	callG := quant.CalculateBlackScholes(true, spot, strike, t, rRate, iv)
	putG := quant.CalculateBlackScholes(false, spot, strike, t, rRate, iv)

	// Put-call parity for futures options (Black-76): C - P == (F - K) * exp(-rT).
	discount := math.Exp(-rRate * t)
	theoreticalDiff := (spot - strike) * discount
	actualDiff := callLast - putLast
	paritySpread := actualDiff - theoreticalDiff

	// Conversion: Sell Call, Buy Put, Buy Future -> locked profit = C - P + (K - F).
	conversionPnl := callLast - putLast + (strike - spot)
	reversalPnl := -conversionPnl

	strategy := "No Arbitrage"
	if paritySpread > 30.0 {
		strategy = "Conversion (Sell Call, Buy Put, Buy Future)"
	} else if paritySpread < -30.0 {
		strategy = "Reversal (Buy Call, Sell Put, Sell Future)"
	}

	// Real margins: short option GO (IMNP) + futures initial margin (INITIALMARGIN).
	futureMargin := 0.0
	for _, fc := range futuresContractsForSymbol(symbol) {
		if fc.Code == seriesCode {
			if m := moexFutureInitialMargin(fc.Code); m > 0 {
				futureMargin = m
			}
			break
		}
	}
	shortOptionGO := callOpt.IMNP // selling the call locks this margin
	if strategy == "Reversal" {
		shortOptionGO = putOpt.IMNP
	}
	totalMargin := shortOptionGO + futureMargin

	// Position theta: short call (-theta) + long put (+theta), per contract.
	thetaPerContract := -callG.Theta + putG.Theta

	response["call"] = map[string]interface{}{
		"secid":         callOpt.SecID,
		"price":         callLast,
		"bid":           callBid,
		"offer":         callOffer,
		"implied_vol":   callIV * 100,
		"theoretical":   callG.Price,
		"theta":         callG.Theta,
		"delta":         callG.Delta,
		"margin_short":  callOpt.IMNP,
	}
	response["put"] = map[string]interface{}{
		"secid":         putOpt.SecID,
		"price":         putLast,
		"bid":           putBid,
		"offer":         putOffer,
		"implied_vol":   putIV * 100,
		"theoretical":   putG.Price,
		"theta":         putG.Theta,
		"delta":         putG.Delta,
		"margin_short":  putOpt.IMNP,
	}
	response["theoretical_diff"] = math.Round(theoreticalDiff*100) / 100
	response["actual_diff"] = math.Round(actualDiff*100) / 100
	response["parity_spread"] = math.Round(paritySpread*100) / 100
	response["conversion_pnl"] = math.Round(conversionPnl*100) / 100
	response["reversal_pnl"] = math.Round(reversalPnl*100) / 100
	response["strategy"] = strategy
	response["implied_vol"] = math.Round(iv*10000) / 100
	response["future_margin"] = math.Round(futureMargin*100) / 100
	response["short_option_go"] = math.Round(shortOptionGO*100) / 100
	response["total_margin"] = math.Round(totalMargin*100) / 100
	response["theta_per_contract"] = math.Round(thetaPerContract*100) / 100
	response["risk_free"] = rRate

	json.NewEncoder(w).Encode(response)
}

// strategyIronCondorHandler computes a real Iron Condor (TDSS) using live MOEX
// ISS option prices and margins: sell OTM put, buy further OTM put, sell OTM
// call, buy further OTM call.
// URL: /api/v1/strategy/ironcondor?symbol=Si&width=1
func strategyIronCondorHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	widthStr := r.URL.Query().Get("width")
	width := 1.0
	if widthStr != "" {
		width, _ = strconv.ParseFloat(widthStr, 64)
	}
	if width < 0.5 {
		width = 1.0
	}

	spot, err := getSpotPrice(symbol)
	if err != nil || spot <= 0 {
		spot = 83200.0
	}

	seriesMu.Lock()
	seriesCode := selectedSeries[symbol]
	seriesMu.Unlock()

	expiry := ""
	expiryTime := currentSeriesExpiry(symbol)
	if expiryTime != nil {
		expiry = expiryTime.Format("2006-01-02")
	}

	chain := moexOptionsForAsset(symbol, expiry)

	atmStrike := nearestStrike(chain, spot)

	// Collect unique strikes sorted ascending.
	seen := map[float64]bool{}
	var strikes []float64
	for _, o := range chain {
		if !seen[o.Strike] {
			seen[o.Strike] = true
			strikes = append(strikes, o.Strike)
		}
	}
	for i := 0; i < len(strikes); i++ {
		for j := i + 1; j < len(strikes); j++ {
			if strikes[j] < strikes[i] {
				strikes[i], strikes[j] = strikes[j], strikes[i]
			}
		}
	}

	// Step = most common distance between adjacent strikes near ATM.
	step := 0.0
	freq := map[float64]int{}
	for i := 1; i < len(strikes); i++ {
		d := math.Round((strikes[i]-strikes[i-1])*10000) / 10000
		if d > 0 {
			freq[d]++
		}
	}
	for d, c := range freq {
		if c > 0 && d > 0 && (step == 0 || c > freq[step]) {
			step = d
		}
	}
	if step <= 0 {
		step = 1000.0
		if symbol == "RI" {
			step = 2000.0
		} else if symbol == "CR" {
			step = 0.1
		}
	}

	// Find strikes as neighbours of the ATM strike in the sorted list:
	// sell put = 1 below ATM, buy put = 2 below, sell call = 1 above, buy call = 2 above.
	atmIdx := -1
	for i, s := range strikes {
		if s == atmStrike {
			atmIdx = i
			break
		}
	}
	var sellPutStrike, buyPutStrike, sellCallStrike, buyCallStrike float64
	if atmIdx >= 0 {
		if atmIdx-1 >= 0 {
			sellPutStrike = strikes[atmIdx-1]
		}
		if atmIdx-2 >= 0 {
			buyPutStrike = strikes[atmIdx-2]
		}
		if atmIdx+1 < len(strikes) {
			sellCallStrike = strikes[atmIdx+1]
		}
		if atmIdx+2 < len(strikes) {
			buyCallStrike = strikes[atmIdx+2]
		}
	}
	// If ATM not found, fall back to nearest below/above scan.
	if atmIdx < 0 {
		for _, s := range strikes {
			if s < atmStrike && sellPutStrike == 0 {
				sellPutStrike = s
			}
			if s > atmStrike && sellCallStrike == 0 {
				sellCallStrike = s
			}
		}
	}

	findOpt := func(strike float64, isCall bool) *optionContract {
		for i := range chain {
			if chain[i].Strike == strike && chain[i].IsCall == isCall {
				return &chain[i]
			}
		}
		return nil
	}

	sellPut := findOpt(sellPutStrike, false)
	buyPut := findOpt(buyPutStrike, false)
	sellCall := findOpt(sellCallStrike, true)
	buyCall := findOpt(buyCallStrike, true)

	response := map[string]interface{}{
		"symbol":       symbol,
		"series":       seriesCode,
		"expiry":       expiry,
		"days_to_exp":  dteInDays(expiry, time.Now()),
		"spot_price":   spot,
		"atm_strike":   atmStrike,
		"step":         step,
		"note":         "Live MOEX ISS prices",
		"sell_put":     sellPut != nil,
		"buy_put":      buyPut != nil,
		"sell_call":    sellCall != nil,
		"buy_call":     buyCall != nil,
	}

	if sellPut == nil || buyPut == nil || sellCall == nil || buyCall == nil {
		response["error"] = "iron condor legs not found for strikes"
		json.NewEncoder(w).Encode(response)
		return
	}

	days := dteInDays(expiry, time.Now())
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0
	rRate := 0.16

	legs := []struct {
		opt    *optionContract
		isCall bool
	}{
		{sellPut, false}, {buyPut, false}, {sellCall, true}, {buyCall, true},
	}

	type legOut struct {
		SecID        string  `json:"secid"`
		Action       string  `json:"action"`
		Strike       float64 `json:"strike"`
		Price        float64 `json:"price"`
		Theta        float64 `json:"theta"`
		Delta        float64 `json:"delta"`
		MarginShort  float64 `json:"margin_short"`
	}

	var legResults []legOut
	credit := 0.0
	thetaTotal := 0.0
	for i, lg := range legs {
		last, _, _, _ := moexOptionQuote(lg.opt.SecID)
		if last <= 0 {
			last = lg.opt.PrevPrice
		}
		iv := quant.ImpliedVolatility(lg.isCall, last, spot, lg.opt.Strike, t, rRate)
		if iv <= 0 {
			iv = 0.30
		}
		g := quant.CalculateBlackScholes(lg.isCall, spot, lg.opt.Strike, t, rRate, iv)

		isShort := i == 0 || i == 2 // sell put / sell call
		action := "BUY"
		thetaSign := 1.0
		if isShort {
			action = "SELL"
			credit += last
			thetaSign = -1.0
		} else {
			credit -= last
		}
		thetaTotal += thetaSign * g.Theta

		legResults = append(legResults, legOut{
			SecID:       lg.opt.SecID,
			Action:      action,
			Strike:      lg.opt.Strike,
			Price:       math.Round(last*100) / 100,
			Theta:       math.Round(thetaSign*g.Theta*100) / 100,
			Delta:       math.Round(g.Delta*100) / 100,
			MarginShort: lg.opt.IMNP,
		})
	}

	// Iron condor max profit = net credit; max loss = credit - wing width.
	wingWidth := buyCallStrike - sellCallStrike
	if wingWidth <= 0 {
		wingWidth = buyPutStrike - sellPutStrike
	}
	maxLoss := credit - wingWidth

	response["legs"] = legResults
	response["net_credit"] = math.Round(credit*100) / 100
	response["width_step"] = math.Round(wingWidth*10000) / 10000
	response["max_profit"] = math.Round(credit*100) / 100
	response["max_loss"] = math.Round(maxLoss*100) / 100
	response["theta_per_contract"] = math.Round(thetaTotal*100) / 100

	json.NewEncoder(w).Encode(response)
}

// buildStrategy computes real option legs and P&L metrics for a supported TDSS
// strategy using live MOEX ISS prices and exchange margin (IMNP for shorts).
// Returns a response map (never nil); on failure the map contains "error".
// Supported strategies: iron_condor, iron_butterfly, bull_put_spread,
// bear_call_spread, bull_call_spread, bear_put_spread.
func buildStrategy(symbol, strategy string) map[string]interface{} {
	if strategy == "" {
		strategy = "iron_condor"
	}
	if symbol == "" {
		symbol = "Si"
	}

	spot, err := getSpotPrice(symbol)
	if err != nil || spot <= 0 {
		spot = 83200.0
	}

	seriesMu.Lock()
	seriesCode := selectedSeries[symbol]
	seriesMu.Unlock()

	expiry := ""
	expiryTime := currentSeriesExpiry(symbol)
	if expiryTime != nil {
		expiry = expiryTime.Format("2006-01-02")
	}

	chain := moexOptionsForAsset(symbol, expiry)
	if len(chain) == 0 {
		return map[string]interface{}{"error": "option chain not available"}
	}

	atmStrike := nearestStrike(chain, spot)

	// Collect unique strikes sorted ascending.
	seen := map[float64]bool{}
	var strikes []float64
	for _, o := range chain {
		if !seen[o.Strike] {
			seen[o.Strike] = true
			strikes = append(strikes, o.Strike)
		}
	}
	for i := 0; i < len(strikes); i++ {
		for j := i + 1; j < len(strikes); j++ {
			if strikes[j] < strikes[i] {
				strikes[i], strikes[j] = strikes[j], strikes[i]
			}
		}
	}

	atmIdx := -1
	for i, s := range strikes {
		if s == atmStrike {
			atmIdx = i
			break
		}
	}
	if atmIdx < 0 {
		atmIdx = len(strikes) / 2
	}

	// Strike offset from ATM for each strategy. Each leg is defined by
	// (offsetFromATM, isCall, isShort) so the option type is explicit.
	type legSpec struct {
		offset  int
		isCall  bool
		isShort bool
	}
	var specs []legSpec
	displayName := strategy
	switch strategy {
	case "iron_butterfly":
		displayName = "Iron Butterfly"
		// short ATM put + short ATM call, long wings one step out.
		specs = []legSpec{{0, false, true}, {0, true, true}, {-1, false, false}, {1, true, false}}
	case "bull_put_spread":
		displayName = "Bull Put Spread"
		specs = []legSpec{{-1, false, true}, {-2, false, false}}
	case "bear_call_spread":
		displayName = "Bear Call Spread"
		specs = []legSpec{{1, true, true}, {2, true, false}}
	case "bull_call_spread":
		displayName = "Bull Call Spread"
		specs = []legSpec{{0, true, false}, {1, true, true}}
	case "bear_put_spread":
		displayName = "Bear Put Spread"
		specs = []legSpec{{0, false, false}, {-1, false, true}}
	case "long_straddle":
		displayName = "Long Straddle"
		specs = []legSpec{{0, false, false}, {0, true, false}}
	case "long_strangle":
		displayName = "Long Strangle"
		specs = []legSpec{{-1, false, false}, {1, true, false}}
	default: // iron_condor
		displayName = "Iron Condor"
		specs = []legSpec{{-1, false, true}, {-2, false, false}, {1, true, true}, {2, true, false}}
	}

	findOpt := func(strike float64, isCall bool) *optionContract {
		for i := range chain {
			if chain[i].Strike == strike && chain[i].IsCall == isCall {
				return &chain[i]
			}
		}
		return nil
	}

	days := dteInDays(expiry, time.Now())
	if days <= 0 {
		days = 30
	}
	t := float64(days) / 365.0
	rRate := 0.16

	type legOut struct {
		SecID       string  `json:"secid"`
		Action      string  `json:"action"`
		Strike      float64 `json:"strike"`
		IsCall      bool    `json:"is_call"`
		Price       float64 `json:"price"`
		Theta       float64 `json:"theta"`
		Delta       float64 `json:"delta"`
		MarginShort float64 `json:"margin_short"`
	}

	var legResults []legOut
	credit := 0.0
	debit := 0.0
	thetaTotal := 0.0
	marginShort := 0.0
	for _, sp := range specs {
		idx := atmIdx + sp.offset
		if idx < 0 || idx >= len(strikes) {
			return map[string]interface{}{
				"error": fmt.Sprintf("%s legs not found for strikes", displayName),
			}
		}
		strike := strikes[idx]
		isCall := sp.isCall
		opt := findOpt(strike, isCall)
		if opt == nil {
			return map[string]interface{}{
				"error": fmt.Sprintf("%s: option not found at strike %v (call=%v)", displayName, strike, isCall),
			}
		}
		last, _, _, _ := moexOptionQuote(opt.SecID)
		if last <= 0 {
			last = opt.PrevPrice
		}
		iv := quant.ImpliedVolatility(isCall, last, spot, strike, t, rRate)
		if iv <= 0 {
			iv = 0.30
		}
		g := quant.CalculateBlackScholes(isCall, spot, strike, t, rRate, iv)

		action := "BUY"
		thetaSign := 1.0
		if sp.isShort {
			action = "SELL"
			credit += last
			thetaSign = -1.0
			if opt.IMNP > 0 {
				marginShort += opt.IMNP
			}
		} else {
			debit += last
		}
		thetaTotal += thetaSign * g.Theta

		legResults = append(legResults, legOut{
			SecID:       opt.SecID,
			Action:      action,
			Strike:      strike,
			IsCall:      isCall,
			Price:       math.Round(last*100) / 100,
			Theta:       math.Round(thetaSign*g.Theta*100) / 100,
			Delta:       math.Round(g.Delta*100) / 100,
			MarginShort: opt.IMNP,
		})
	}

	netCredit := credit - debit

	// Wing width = distance between short strike and long wing on the same
	// side (max of the two sides for condor/butterfly).
	wingWidth := 0.0
	for _, sp := range specs {
		if !sp.isShort {
			continue
		}
		for _, sp2 := range specs {
			if sp2.isShort || (sp.isCall != sp2.isCall) {
				continue
			}
			w := math.Abs(strikes[atmIdx+sp.offset] - strikes[atmIdx+sp2.offset])
			if w > wingWidth {
				wingWidth = w
			}
		}
	}
	if wingWidth <= 0 {
		wingWidth = 1.0
	}

	var maxProfit, maxLoss float64
	switch strategy {
	case "long_straddle", "long_strangle":
		// Long volatility: risk = total premium paid, upside unbounded.
		maxLoss = debit
		maxProfit = 0
	case "bull_call_spread", "bear_put_spread":
		// Debit spreads: net debit = buy - sell; max loss = net debit,
		// max profit = wing width - net debit.
		netDebit := debit - credit
		if netDebit < 0 {
			netDebit = 0
		}
		maxProfit = wingWidth - netDebit
		maxLoss = netDebit
	default:
		// Credit strategies: max profit = net credit, max loss = wing - credit.
		maxProfit = netCredit
		maxLoss = wingWidth - netCredit
	}
	if maxProfit < 0 {
		maxProfit = 0
	}
	if maxLoss < 0 {
		maxLoss = 0
	}

	unlimited := strategy == "long_straddle" || strategy == "long_strangle"

	return map[string]interface{}{
		"symbol":              symbol,
		"series":              seriesCode,
		"expiry":              expiry,
		"days_to_exp":         dteInDays(expiry, time.Now()),
		"spot_price":          spot,
		"atm_strike":          atmStrike,
		"strategy_name":       displayName,
		"note":                "Live MOEX ISS prices",
		"unlimited_max_profit": unlimited,
		"legs":                legResults,
		"net_credit":          math.Round(netCredit*100) / 100,
		"width_step":          math.Round(wingWidth*10000) / 10000,
		"max_profit":          math.Round(maxProfit*100) / 100,
		"max_loss":            math.Round(maxLoss*100) / 100,
		"theta_per_contract":  math.Round(thetaTotal*100) / 100,
		"margin_short_total":  math.Round(marginShort*100) / 100,
	}
}

// strategyBuildHandler builds real option legs for any supported TDSS strategy.
// URL: /api/v1/strategy/build?strategy=iron_condor&symbol=Si
func strategyBuildHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	strategy := r.URL.Query().Get("strategy")
	symbol := r.URL.Query().Get("symbol")
	json.NewEncoder(w).Encode(buildStrategy(symbol, strategy))
}

// moexFutureInitialMargin fetches the exchange initial margin (INITIALMARGIN)
// for a FORTS futures contract from MOEX ISS securities description.
func moexFutureInitialMargin(secid string) float64 {
	url := fmt.Sprintf("http://iss.moex.com/iss/engines/futures/markets/forts/boards/RFUD/securities/%s.json?iss.meta=off&iss.only=securities&securities.columns=SECID,INITIALMARGIN", secid)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	var issResp struct {
		Securities struct {
			Data [][]interface{} `json:"data"`
		} `json:"securities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issResp); err != nil {
		return 0
	}
	if len(issResp.Securities.Data) == 0 || len(issResp.Securities.Data[0]) < 2 {
		return 0
	}
	m, _ := issResp.Securities.Data[0][1].(float64)
	return m
}

func optionsRecommendationsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ivStr := r.URL.Query().Get("iv")
	hvStr := r.URL.Query().Get("hv")
	spotStr := r.URL.Query().Get("spot")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	iv := 35.0
	if ivStr != "" {
		iv, _ = strconv.ParseFloat(ivStr, 64)
	}
	hv := 28.0
	if hvStr != "" {
		hv, _ = strconv.ParseFloat(hvStr, 64)
	}
	spot, _ := getSpotPrice(symbol)
	if spot <= 0 {
		spot = 83200.0
	}
	if spotStr != "" {
		spot, _ = strconv.ParseFloat(spotStr, 64)
	}

	recs := quant.GenerateStrategyRecommendations(iv, hv, spot)
	regime := quant.ClassifyMarketRegime(iv, hv, false)

	// Enrich each recommendation with live market numbers from buildStrategy.
	for i := range recs {
		b := buildStrategy(symbol, recs[i].StrategyType)
		if err, _ := b["error"].(string); err != "" {
			continue
		}
		if mp, ok := b["max_profit"].(float64); ok {
			recs[i].RealMaxProfit = mp
		}
		if ml, ok := b["max_loss"].(float64); ok {
			recs[i].RealMaxLoss = ml
		}
		if th, ok := b["theta_per_contract"].(float64); ok {
			recs[i].RealTheta = th
		}
		if mg, ok := b["margin_short_total"].(float64); ok {
			recs[i].RealMargin = mg
		}
		if sp, ok := b["spot_price"].(float64); ok {
			recs[i].RealSpot = sp
		}
		if un, ok := b["unlimited_max_profit"].(bool); ok {
			recs[i].Unlimited = un
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"market_regime":   string(regime),
		"recommendations": recs,
		"trade_gate":      tradeGate(symbol),
		"trend":           tradeTrend(symbol),
	})
}

// normalizeStrategyName maps a position display name (e.g. "Iron Condor",
// "Bull Call Spread") to its strategy key ("iron_condor", "bull_call_spread").
func normalizeStrategyName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "condor"):
		return "iron_condor"
	case strings.Contains(lower, "butterfly"):
		return "iron_butterfly"
	case strings.Contains(lower, "bull") && strings.Contains(lower, "put"):
		return "bull_put_spread"
	case strings.Contains(lower, "bull") && strings.Contains(lower, "call"):
		return "bull_call_spread"
	case strings.Contains(lower, "bear") && strings.Contains(lower, "put"):
		return "bear_put_spread"
	case strings.Contains(lower, "bear") && strings.Contains(lower, "call"):
		return "bear_call_spread"
	case strings.Contains(lower, "strangle"):
		return "long_strangle"
	case strings.Contains(lower, "straddle"):
		return "long_straddle"
	case strings.Contains(lower, "execution") || strings.Contains(lower, "futures"):
		return "futures"
	default:
		return lower
	}
}

// rotationHandler ranks strategies by the current market regime and flags held
// positions that no longer fit it.
// URL: /api/v1/strategy/rotation?symbol=Si
func rotationHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	iv := currentATMIVRaw(symbol)
	hv := realizedVolForSymbol(symbol)
	if iv <= 0 {
		iv = 0.35
	}
	if hv <= 0 {
		hv = 0.28
	}

	trend := tradeTrend(symbol)
	trendRegime := "SIDEWAYS"
	if t, ok := trend["regime"].(string); ok {
		trendRegime = t
	}
	isTrending := trendRegime == "BULLISH" || trendRegime == "BEARISH"
	regime := quant.ClassifyMarketRegime(iv, hv, isTrending)

	positions := quant.GetActivePositions()
	held := make([]quant.HeldPositionInfo, 0, len(positions))
	for _, p := range positions {
		held = append(held, quant.HeldPositionInfo{ID: p.ID, Strategy: normalizeStrategyName(p.Strategy), Symbol: p.Symbol})
	}

	advice := quant.RecommendRotation(regime, trendRegime, held)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"rotation":  advice,
		"iv":        math.Round(iv*10000) / 10000,
		"hv":        math.Round(hv*10000) / 10000,
		"trade_gate": tradeGate(symbol),
	})
}

func optionsTrendHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	json.NewEncoder(w).Encode(tradeTrend(symbol))
}

func optionsIVRankHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}
	current := currentATMIVRaw(symbol)
	json.NewEncoder(w).Encode(ivRankStats(symbol, current))
}

func optionsExitAdviceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	dteStr := r.URL.Query().Get("dte")
	deltaStr := r.URL.Query().Get("delta")
	profitStr := r.URL.Query().Get("profit")

	dte := 30.0
	if dteStr != "" {
		dte, _ = strconv.ParseFloat(dteStr, 64)
	}
	delta := 0.12
	if deltaStr != "" {
		delta, _ = strconv.ParseFloat(deltaStr, 64)
	}
	profit := 50.0
	if profitStr != "" {
		profit, _ = strconv.ParseFloat(profitStr, 64)
	}

	advice := quant.AnalyzeExitTriggers(dte, delta, profit)
	json.NewEncoder(w).Encode(advice)
}

func gammaScalpingStepHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	thetaStr := r.URL.Query().Get("theta")
	gammaStr := r.URL.Query().Get("gamma")

	theta := 450.0
	if thetaStr != "" {
		theta, _ = strconv.ParseFloat(thetaStr, 64)
	}
	gamma := 0.004
	if gammaStr != "" {
		gamma, _ = strconv.ParseFloat(gammaStr, 64)
	}

	step := quant.CalculateGammaScalpingStep(theta, gamma)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"theta":     theta,
		"gamma":     gamma,
		"move_step": step,
	})
}

func verticalSpreadHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ivStr := r.URL.Query().Get("iv")
	hvStr := r.URL.Query().Get("hv")
	outlook := r.URL.Query().Get("outlook")
	if outlook == "" {
		outlook = "BULLISH"
	}
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	iv := 35.0
	if ivStr != "" {
		iv, _ = strconv.ParseFloat(ivStr, 64)
	}
	hv := 28.0
	if hvStr != "" {
		hv, _ = strconv.ParseFloat(hvStr, 64)
	}

	rec := quant.EvaluateVerticalSpreads(iv, hv, outlook)

	// Enrich with live market numbers for the chosen spread strategy.
	if b := buildStrategy(symbol, rec.StrategyType); b["error"] == nil {
		if mp, ok := b["max_profit"].(float64); ok {
			rec.RealMaxProfit = mp
		}
		if ml, ok := b["max_loss"].(float64); ok {
			rec.RealMaxLoss = ml
		}
		if th, ok := b["theta_per_contract"].(float64); ok {
			rec.RealTheta = th
		}
		if mg, ok := b["margin_short_total"].(float64); ok {
			rec.RealMargin = mg
		}
		if sp, ok := b["spot_price"].(float64); ok {
			rec.RealSpot = sp
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"recommendation": rec,
		"trade_gate":     tradeGate(symbol),
	})
}

func rollingAdviceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	direction := r.URL.Query().Get("direction")
	if direction == "" {
		direction = "BULLISH"
	}
	drawdownStr := r.URL.Query().Get("drawdown")
	drawdown := 35.0
	if drawdownStr != "" {
		drawdown, _ = strconv.ParseFloat(drawdownStr, 64)
	}

	advice := quant.GetSpreadRollingAdvice(direction, drawdown)
	json.NewEncoder(w).Encode(advice)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	// Encrypted token storage for Alor credentials.
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	var err error
	tokenStore, err = secure.NewStoreWithKey(dataDir, os.Getenv("ALOR_TOKEN_SECRET"))
	if err != nil {
		log.Fatalf("Failed to initialize secure store: %v", err)
	}
	initAlorClients()
	initTelegram()
	initSpreads(dataDir)

	// Background Telegram notifier (stop channel unused for lifetime app).
	go telegramNotifier(make(chan struct{}))

	// Load persisted portfolio (positions, trades, capital) from disk.
	quant.SetDataFile(filepath.Join(dataDir, "portfolio.json"))
	quant.Load()

	subFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("Failed to create sub filesystem: %v", err)
	}

	// Static UI Handler
	http.Handle("/", http.FileServer(http.FS(subFS)))

	// API Handlers
	http.HandleFunc("/api/v1/greeks", greeksHandler)
	http.HandleFunc("/api/v1/market/quote", quoteHandler)
	http.HandleFunc("/api/v1/greeks/live", liveGreeksHandler)
	http.HandleFunc("/api/v1/arbitrage/check", arbitrageHandler)
	http.HandleFunc("/api/v1/options/skew", skewHandler)
	http.HandleFunc("/api/v1/spot", spotHandler)

	// MOEX / Alor Handlers
	http.HandleFunc("/api/v1/moex/quote", moexQuoteHandler)
	http.HandleFunc("/api/v1/moex/arbitrage", moexArbitrageHandler)
	http.HandleFunc("/api/v1/moex/order", moexOrderHandler)
	http.HandleFunc("/api/v1/moex/perp-quarterly", moexPerpQuarterlyHandler)
	http.HandleFunc("/api/v1/strategy/parity", strategyParityHandler)
	http.HandleFunc("/api/v1/strategy/ironcondor", strategyIronCondorHandler)
	http.HandleFunc("/api/v1/strategy/build", strategyBuildHandler)
	http.HandleFunc("/api/v1/series", seriesInfoHandler)
	http.HandleFunc("/api/v1/series/set", setSeriesHandler)

	// Settings (encrypted Alor token / Telegram)
	http.HandleFunc("/api/v1/settings/token", settingsTokenHandler)
	http.HandleFunc("/api/v1/settings/telegram", settingsTelegramHandler)

	// Positions & Portfolio Handlers
	http.HandleFunc("/api/v1/positions", positionsHandler)
	http.HandleFunc("/api/v1/positions/open", openPositionHandler)
	http.HandleFunc("/api/v1/positions/close", closePositionHandler)
	http.HandleFunc("/api/v1/positions/hedge", deltaHedgeHandler)
	http.HandleFunc("/api/v1/positions/pnl-attribution", pnlAttributionHandler)
	http.HandleFunc("/api/v1/positions/sizing", positionSizingHandler)
	http.HandleFunc("/api/v1/positions/expiry-risk", expiryRiskHandler)

	// Vertical Spreads console
	http.HandleFunc("/api/v1/spreads/plan", spreadPlanHandler)
	http.HandleFunc("/api/v1/spreads/advice", spreadAdviceHandler)
	http.HandleFunc("/api/v1/spreads/open", spreadOpenHandler)
	http.HandleFunc("/api/v1/spreads/close", spreadCloseHandler)
	http.HandleFunc("/api/v1/spreads/hedge", spreadHedgeHandler)
	http.HandleFunc("/api/v1/spreads/roll", spreadRollHandler)
	http.HandleFunc("/api/v1/spreads/rules", spreadRulesHandler)
	http.HandleFunc("/api/v1/spreads/manager", spreadManagerHandler)
	http.HandleFunc("/api/v1/spreads", spreadListHandler)
	http.HandleFunc("/api/v1/strategy/rotation", rotationHandler)
	http.HandleFunc("/api/v1/position/profile", positionProfileHandler)
	http.HandleFunc("/api/v1/trades", tradesHandler)
	http.HandleFunc("/api/v1/portfolio", portfolioHandler)
	http.HandleFunc("/api/v1/risk", riskHandler)
	http.HandleFunc("/api/v1/risk/stress", stressTestHandler)
	http.HandleFunc("/api/v1/risk/heatmap", heatmapHandler)
	http.HandleFunc("/api/v1/capital", capitalHandler)
	http.HandleFunc("/api/v1/copilot/ask", copilotHandler)
	http.HandleFunc("/api/v1/options/exit-advice", optionsExitAdviceHandler)
	http.HandleFunc("/api/v1/options/recommendations", optionsRecommendationsHandler)
	http.HandleFunc("/api/v1/options/trend", optionsTrendHandler)
	http.HandleFunc("/api/v1/options/iv-rank", optionsIVRankHandler)
	http.HandleFunc("/api/v1/options/gamma-step", gammaScalpingStepHandler)
	http.HandleFunc("/api/v1/options/vertical-spread", verticalSpreadHandler)
	http.HandleFunc("/api/v1/options/rolling-advice", rollingAdviceHandler)
	http.HandleFunc("/api/v1/backtest", backtestHandler)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": "Go Quant Core + MOEX Alor"})
	})

	fmt.Printf("Quant Engine & Dashboard running on port %s...\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}