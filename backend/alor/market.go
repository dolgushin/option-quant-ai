package alor

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type MarketClient struct {
	authClient *AuthClient
	baseURL    string
	mu         sync.Mutex
	lastCall   time.Time
}

type SecurityQuote struct {
	Symbol      string    `json:"symbol"`
	Exchange    string    `json:"exchange"`
	Description string    `json:"description"`
	Price       float64   `json:"price"`
	Bid         float64   `json:"bid"`
	Ask         float64   `json:"ask"`
	Volume      float64   `json:"volume"`
	Timestamp   time.Time `json:"timestamp"`
}

type AlorSecurityResponse struct {
	Symbol      string  `json:"symbol"`
	ShortName   string  `json:"shortname"`
	Description string  `json:"description"`
	Exchange    string  `json:"exchange"`
	Price       float64 `json:"last_price"`
	High        float64 `json:"high"`
	Low         float64 `json:"low"`
	Volume      float64 `json:"volume"`
}

type AlorOrderbookResponse struct {
	Bids []OrderbookEntry `json:"bids"`
	Asks []OrderbookEntry `json:"asks"`
}

type OrderbookEntry struct {
	Price  float64 `json:"price"`
	Volume int     `json:"volume"`
}

// alorMonthNums maps MOEX futures month letters to calendar months.
var alorMonthNums = map[byte]int{
	'F': 1, 'G': 2, 'H': 3, 'J': 4, 'K': 5, 'M': 6,
	'N': 7, 'Q': 8, 'U': 9, 'V': 10, 'X': 11, 'Z': 12,
}

// alorTradeRoots are underlyings whose month-code futures must be rewritten.
// Alor 02.10.2026: SiZ6-style codes don't exist — every such request errors
// (499) and counts toward the rate ban. Si-12.26 is the valid form.
var alorTradeRoots = []string{"Si", "RI"}

// normalizeAlorSecID rewrites MOEX month-code futures (SiZ6) to Alor
// instrument format (Si-12.26) at the client boundary, so every caller is
// fixed at once. Options (Si86000BJ6), stocks and qualified codes pass
// through unchanged; internal records keep month-code form so hedge
// netting/matching stays on one canonical spelling. Pure — unit-tested.
func normalizeAlorSecID(secid string, now time.Time) string {
	for _, root := range alorTradeRoots {
		rest, found := strings.CutPrefix(secid, root)
		if !found || len(rest) != 2 {
			continue
		}
		mo, ok := alorMonthNums[rest[0]]
		if !ok || rest[1] < '0' || rest[1] > '9' {
			continue
		}
		// No decade roll: listed futures live ≤ 1.5y, the digit maps into
		// the current decade (a past contract stays past — correctly dead).
		y := now.Year() - now.Year()%10 + int(rest[1]-'0')
		return fmt.Sprintf("%s-%02d.%02d", root, mo, y%100)
	}
	return secid
}

// alorMinInterval caps the client at ~10 req/s sustained (Alor limit 100/s).
// Concurrent reprices serialize here instead of fanning out.
const alorMinInterval = 100 * time.Millisecond

func (m *MarketClient) throttle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d := alorMinInterval - time.Since(m.lastCall); d > 0 {
		time.Sleep(d)
	}
	m.lastCall = time.Now()
}

// FetchOrderbook returns the full limit order book (all levels) for a MOEX
// instrument via Alor. An empty exchange defaults to MOEX.
func (m *MarketClient) FetchOrderbook(exchange, symbol string) (AlorOrderbookResponse, error) {
	var empty AlorOrderbookResponse
	m.throttle()
	symbol = normalizeAlorSecID(symbol, time.Now())
	if exchange == "" {
		exchange = "MOEX"
	}
	token, err := m.authClient.GetAccessToken()
	if err != nil {
		return empty, fmt.Errorf("authentication error: %w", err)
	}
	url := fmt.Sprintf("%s/md/v2/orderbooks/%s/%s", m.baseURL, exchange, symbol)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return empty, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return empty, fmt.Errorf("failed to fetch orderbook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("alor orderbook %s: status %d", symbol, resp.StatusCode)
		return empty, fmt.Errorf("alor orderbook API returned status: %d", resp.StatusCode)
	}
	var ob AlorOrderbookResponse
	if err := json.NewDecoder(resp.Body).Decode(&ob); err != nil {
		return empty, fmt.Errorf("failed to decode orderbook: %w", err)
	}
	return ob, nil
}

func NewMarketClient(authClient *AuthClient) *MarketClient {
	return &MarketClient{
		authClient: authClient,
		baseURL:    "https://api.alor.ru",
	}
}

// FetchSecurityQuote fetches real-time quote for MOEX FORTS asset (e.g., Si-12.26 or RI-12.26 or option contract)
func (m *MarketClient) FetchSecurityQuote(symbol string) (*SecurityQuote, error) {
	m.throttle()
	symbol = normalizeAlorSecID(symbol, time.Now())
	token, err := m.authClient.GetAccessToken()
	if err != nil {
		return nil, fmt.Errorf("authentication error: %w", err)
	}

	url := fmt.Sprintf("%s/md/v2/Securities/MOEX/%s", m.baseURL, symbol)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch security from Alor API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("alor securities %s: status %d", symbol, resp.StatusCode)
		return nil, fmt.Errorf("alor securities API returned status: %d", resp.StatusCode)
	}

	var sec AlorSecurityResponse
	if err := json.NewDecoder(resp.Body).Decode(&sec); err != nil {
		return nil, fmt.Errorf("failed to decode security response: %w", err)
	}

	// Fetch orderbook for best bid/ask
	bid, ask := m.fetchBestBidAsk("MOEX", symbol, token)

	price := sec.Price
	if price == 0 {
		price = (bid + ask) / 2.0
	}

	return &SecurityQuote{
		Symbol:      symbol,
		Exchange:    "MOEX",
		Description: sec.Description,
		Price:       price,
		Bid:         bid,
		Ask:         ask,
		Volume:      sec.Volume,
		Timestamp:   time.Now(),
	}, nil
}

func (m *MarketClient) fetchBestBidAsk(exchange, symbol, token string) (float64, float64) {
	m.throttle()
	symbol = normalizeAlorSecID(symbol, time.Now())
	url := fmt.Sprintf("%s/md/v2/orderbooks/%s/%s", m.baseURL, exchange, symbol)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0
	}

	var ob AlorOrderbookResponse
	if err := json.NewDecoder(resp.Body).Decode(&ob); err != nil {
		return 0, 0
	}

	var bestBid, bestAsk float64
	if len(ob.Bids) > 0 {
		bestBid = ob.Bids[0].Price
	}
	if len(ob.Asks) > 0 {
		bestAsk = ob.Asks[0].Price
	}

	return bestBid, bestAsk
}

// RawGet performs an authenticated GET against path+query on the Alor API
// and returns status plus raw body. Diagnostic passthrough for discovering
// response shapes (boards, history) without baking in guesses.
func (m *MarketClient) RawGet(path, rawQuery string) (int, []byte, error) {
	m.throttle()
	token, err := m.authClient.GetAccessToken()
	if err != nil {
		return 0, nil, err
	}
	url := fmt.Sprintf("%s%s", m.baseURL, path)
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// FetchOptionChain fetches option symbols or derivatives for underlying root (Si or RI)
func (m *MarketClient) FetchOptionChain(rootSymbol string) ([]string, error) {
	m.throttle()
	token, err := m.authClient.GetAccessToken()
	if err != nil {
		return nil, err
	}

	// Alor instruments search endpoint
	url := fmt.Sprintf("%s/md/v2/Securities/MOEX?query=%s", m.baseURL, rootSymbol)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("alor search %s: status %d", rootSymbol, resp.StatusCode)
		return nil, fmt.Errorf("failed to search instruments: status %d", resp.StatusCode)
	}

	var securities []AlorSecurityResponse
	if err := json.NewDecoder(resp.Body).Decode(&securities); err != nil {
		// might be single object or array
		return []string{rootSymbol + "C50000"}, nil
	}

	var symbols []string
	for _, s := range securities {
		symbols = append(symbols, s.Symbol)
	}

	if len(symbols) == 0 {
		symbols = []string{rootSymbol + "C50000", rootSymbol + "P50000"}
	}

	return symbols, nil
}
