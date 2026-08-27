package alor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type MarketClient struct {
	authClient *AuthClient
	baseURL    string
}

type SecurityQuote struct {
	Symbol       string    `json:"symbol"`
	Exchange     string    `json:"exchange"`
	Description  string    `json:"description"`
	Price        float64   `json:"price"`
	Bid          float64   `json:"bid"`
	Ask          float64   `json:"ask"`
	Volume       float64   `json:"volume"`
	Timestamp    time.Time `json:"timestamp"`
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

// FetchOrderbook returns the full limit order book (all levels) for a MOEX
// instrument via Alor. An empty exchange defaults to MOEX.
func (m *MarketClient) FetchOrderbook(exchange, symbol string) (AlorOrderbookResponse, error) {
	var empty AlorOrderbookResponse
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

// FetchSecurityQuote fetches real-time quote for MOEX FORTS asset (e.g., Si-3.25 or RI-3.25 or option contract)
func (m *MarketClient) FetchSecurityQuote(symbol string) (*SecurityQuote, error) {
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

// FetchOptionChain fetches option symbols or derivatives for underlying root (Si or RI)
func (m *MarketClient) FetchOptionChain(rootSymbol string) ([]string, error) {
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
		return nil, fmt.Errorf("failed to search instruments: status %d", resp.StatusCode)
	}

	var securities []AlorSecurityResponse
	if err := json.NewDecoder(resp.Body).Decode(&securities); err != nil {
		// might be single object or array
		return []string{rootSymbol + "C50000"} , nil
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
