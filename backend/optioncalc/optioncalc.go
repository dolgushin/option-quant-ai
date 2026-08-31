// Package optioncalc is a small typed client for the MOEX Options Calculator
// REST API (https://iss.moex.com/iss/apps/option-calc/v1). It returns the
// exchange's own theo prices, implied volatilities and greeks instead of
// re-deriving them locally, so charts and analytics match the MOEX constructor.
package optioncalc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// baseURL is the public (no-auth) Options Calculator API host.
const baseURL = "https://iss.moex.com/iss/apps/option-calc/v1"

// Client fetches and (shortly) caches Options Calculator responses.
type Client struct {
	http *http.Client
	mu   sync.Mutex
	ttl  time.Duration
	cch  map[string]cacheEntry
}

type cacheEntry struct {
	data []byte
	ttl  time.Time
}

// New returns a Client with per-URL TTL cache (default 60s) and a generous
// timeout (the calculator can be slow computing a full board).
func New() *Client {
	return &Client{
		http: &http.Client{Timeout: 20 * time.Second},
		ttl:  60 * time.Second,
		cch:  map[string]cacheEntry{},
	}
}

// Series is one option series (one expiry on one asset) from the calculator.
type Series struct {
	Code          string  `json:"optionseries_code"`
	AssetCode     string  `json:"asset_code"`
	FuturesCode   string  `json:"futures_code"`
	SeriesType    string  `json:"series_type"`
	Expiration    string  `json:"expiration_date"`
	CentralStrike float64 `json:"central_strike"`
}

// BoardOption is a single option in a board row with the exchange's greeks.
type BoardOption struct {
	SecID      string  `json:"secid"`
	Strike     float64 `json:"strike"`
	Theo       float64 `json:"theorprice"`
	TheoRub    float64 `json:"theorprice_rub"`
	Last       float64 `json:"last"`
	Offer      float64 `json:"offer"`
	Bid        float64 `json:"bid"`
	NumTrades  int     `json:"numtrades"`
	Volatility float64 `json:"volatility"` // percent
	Intrinsic  float64 `json:"intrinsic_value"`
	TimedValue float64 `json:"timed_value"`
	Delta      float64 `json:"delta"`
	Gamma      float64 `json:"gamma"`
	Vega       float64 `json:"vega"`
	Theta      float64 `json:"theta"`
	Rho        float64 `json:"rho"`
}

// Board is the full option board (calls + puts) for a series.
type Board struct {
	Calls []BoardOption `json:"call"`
	Puts  []BoardOption `json:"put"`
}

// VolatilityPoint is a (strike, iv) point on the series' volatility graph.
type VolatilityPoint struct {
	Strike float64 `json:"strike"`
	Vol    float64 `json:"volatility"`
}

// Brief is the calculator's per-option computation at caller-supplied inputs
// (custom underlying price, volatility, days to expiry).
type Brief struct {
	Delta             float64 `json:"delta"`
	Gamma             float64 `json:"gamma"`
	Vega              float64 `json:"vega"`
	Theta             float64 `json:"theta"`
	Rho               float64 `json:"rho"`
	SecID             string  `json:"secid"`
	DaysUntilExpiring int     `json:"days_until_expiring"`
	UnderlyingPrice   float64 `json:"underlying_price"`
	Volatility        float64 `json:"volatility"`
	Theo              float64 `json:"theorprice"`
	LastPrice         float64 `json:"lastprice"`
	SettlePrice       float64 `json:"settleprice"`
	ExpiringDate      string  `json:"expiring_date"`
}

func (c *Client) get(path string, query url.Values) ([]byte, error) {
	c.mu.Lock()
	if e, ok := c.cch[path]; ok && time.Now().Before(e.ttl) {
		c.mu.Unlock()
		return e.data, nil
	}
	c.mu.Unlock()

	if query == nil {
		query = url.Values{}
	}
	u := baseURL + path
	enc := query.Encode()
	if enc != "" {
		u += "?" + enc
	}
	resp, err := c.http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("optioncalc request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("optioncalc %s status %d", path, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("optioncalc read failed: %w", err)
	}
	c.mu.Lock()
	c.cch[path] = cacheEntry{data: data, ttl: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return data, nil
}

func (c *Client) getJSON(path string, query url.Values, out interface{}) error {
	data, err := c.get(path, query)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// OptionSeries lists all series for an asset (one per expiry).
func (c *Client) OptionSeries(asset string) ([]Series, error) {
	var out []Series
	q := url.Values{"asset_type": {"futures"}}
	if err := c.getJSON("/assets/"+url.PathEscape(asset)+"/optionseries", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SeriesByExpiry returns the series code whose expiration_date matches expiry.
func (c *Client) SeriesByExpiry(asset, expiry string) (string, error) {
	series, err := c.OptionSeries(asset)
	if err != nil {
		return "", err
	}
	for _, s := range series {
		if s.Expiration == expiry {
			return s.Code, nil
		}
	}
	return "", fmt.Errorf("optioncalc: no series for %s at %s", asset, expiry)
}

// Board returns the option board for a series code.
func (c *Client) Board(asset, seriesCode string) (*Board, error) {
	var out Board
	q := url.Values{"asset_type": {"futures"}}
	if err := c.getJSON("/assets/"+url.PathEscape(asset)+"/optionseries/"+url.PathEscape(seriesCode)+"/optionboard", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VolatilityGraph returns (strike, iv) points for a series' volatility curve.
func (c *Client) VolatilityGraph(asset, seriesCode string) ([]VolatilityPoint, error) {
	var out []VolatilityPoint
	q := url.Values{"asset_type": {"futures"}}
	if err := c.getJSON("/assets/"+url.PathEscape(asset)+"/optionseries/"+url.PathEscape(seriesCode)+"/volatility_graph", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Brief computes the exchange's theo price and greeks for one option at custom
// underlying price, volatility (percent) and days to expiry.
func (c *Client) Brief(asset, secid string, days int, underlyingPrice, volPct float64) (*Brief, error) {
	var out Brief
	q := url.Values{
		"asset_type":          {"futures"},
		"days_until_expiring": {fmt.Sprintf("%d", days)},
		"underlying_price":    {fmt.Sprintf("%g", underlyingPrice)},
		"volatility":          {fmt.Sprintf("%g", volPct)},
	}
	if err := c.getJSON("/assets/"+url.PathEscape(asset)+"/options/"+url.PathEscape(secid), q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
