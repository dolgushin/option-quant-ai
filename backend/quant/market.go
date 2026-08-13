package quant

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type MarketQuote struct {
	Symbol    string    `json:"symbol"`
	Price     float64   `json:"price"`
	Timestamp time.Time `json:"timestamp"`
}

type DeribitTickerResponse struct {
	Result struct {
		IndexName    string  `json:"index_name"`
		LastPrice    float64 `json:"last_price"`
		MarkPrice    float64 `json:"mark_price"`
		EstimatedPrice float64 `json:"estimated_delivery_price"`
	} `json:"result"`
}

// FetchCryptoSpot получает текущую спотовую цену BTC или ETH с Deribit API
func FetchCryptoSpot(symbol string) (*MarketQuote, error) {
	var indexName string
	switch symbol {
	case "BTC":
		indexName = "btc_usd"
	case "ETH":
		indexName = "eth_usd"
	default:
		return nil, fmt.Errorf("unsupported crypto symbol: %s", symbol)
	}

	url := fmt.Sprintf("https://www.deribit.com/api/v2/public/get_index_price?index_name=%s", indexName)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch price from Deribit: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.fmt.Errorf("Deribit API returned status code: %d", resp.StatusCode)
	}

	var result DeribitTickerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode Deribit response: %w", err)
	}

	return &MarketQuote{
		Symbol:    symbol,
		Price:     result.Result.IndexNamePrice(),
		Timestamp: time.Now(),
	}, nil
}

func (r *DeribitTickerResponse) IndexNamePrice() float64 {
	if r.Result.EstimatedPrice > 0 {
		return r.Result.EstimatedPrice
	}
	return r.Result.MarkPrice
}