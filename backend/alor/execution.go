package alor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

type ExecutionClient struct {
	authClient *AuthClient
	baseURL    string
	portfolio  string
	exchange   string
}

// SetPortfolio updates the Alor portfolio at runtime.
func (e *ExecutionClient) SetPortfolio(portfolio string) {
	e.portfolio = portfolio
}

type OrderRequest struct {
	Instrument struct {
		Symbol   string `json:"symbol"`
		Exchange string `json:"exchange"`
	} `json:"instrument"`
	User struct {
		Portfolio string `json:"portfolio"`
	} `json:"user"`
	Side     string  `json:"side"`     // "buy" or "sell"
	Type     string  `json:"type"`     // "market" or "limit"
	Price    float64 `json:"price"`    // required for limit
	Quantity int     `json:"quantity"` // number of contracts
}

type OrderResponse struct {
	OrderNumber string `json:"orderNumber"`
	Message     string `json:"message"`
	Success     bool   `json:"success"`
}

func NewExecutionClient(authClient *AuthClient, portfolio string) *ExecutionClient {
	return &ExecutionClient{
		authClient: authClient,
		baseURL:    "https://api.alor.ru",
		portfolio:  portfolio,
		exchange:   "MOEX",
	}
}

// PlaceOrder sends a limit or market order via Alor Command API v2
func (e *ExecutionClient) PlaceOrder(symbol, side, orderType string, price float64, quantity int) (*OrderResponse, error) {
	token, err := e.authClient.GetAccessToken()
	if err != nil {
		return nil, fmt.Errorf("auth error: %w", err)
	}

	var endpoint string
	if orderType == "market" {
		endpoint = fmt.Sprintf("%s/command/v2/orders/market", e.baseURL)
	} else {
		endpoint = fmt.Sprintf("%s/command/v2/orders/limit", e.baseURL)
	}

	portfolio := e.portfolio
	if portfolio == "" {
		portfolio = "TEST_PORTFOLIO"
	}

	var reqBody OrderRequest
	reqBody.Instrument.Symbol = symbol
	reqBody.Instrument.Exchange = e.exchange
	reqBody.User.Portfolio = portfolio
	reqBody.Side = side
	reqBody.Type = orderType
	reqBody.Price = price
	reqBody.Quantity = quantity

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send order to Alor: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("alor order API error (status %d): %s", resp.StatusCode, buf.String())
	}

	var orderRes OrderResponse
	if err := json.NewDecoder(resp.Body).Decode(&orderRes); err != nil {
		// If response is order number string
		return &OrderResponse{Success: true, Message: "Order submitted successfully"}, nil
	}

	orderRes.Success = true
	return &orderRes, nil
}

// DeltaHedge executes automatic delta-hedging using Si or RI futures
func (e *ExecutionClient) DeltaHedge(futureSymbol string, netDelta float64) (*OrderResponse, error) {
	contracts := int(math.Round(math.Abs(netDelta)))
	if contracts == 0 {
		return &OrderResponse{Success: true, Message: "No hedging needed (delta close to zero)"}, nil
	}

	side := "buy"
	if netDelta > 0 {
		side = "sell" // If portfolio is net long delta, sell futures to neutralize
	} else {
		side = "buy"  // If portfolio is net short delta, buy futures to neutralize
	}

	return e.PlaceOrder(futureSymbol, side, "market", 0, contracts)
}
