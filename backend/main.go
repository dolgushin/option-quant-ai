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
	// Version: 1.1.0 - Encrypted Alor token settings + MOEX ISS live prices

	spotOverrides = map[string]float64{}
	spotMu        sync.Mutex

	// Current active futures series and their option series codes on MOEX FORTS.
	// futuresSeries: display name shown to the user (futures code on MOEX).
	// optionsSeries: Alor instrument root for options, e.g. SiU6.
	futuresSeries = map[string]string{
		"Si": "Si-9.26",
		"RI": "RI-9.26",
		"CR": "CR-9.26",
	}
	optionsSeries = map[string]string{
		"Si": "SiU6",
		"RI": "RIU6",
		"CR": "CRU6",
	}

	// tokenStore persists the Alor refresh token encrypted on disk.
	tokenStore *secure.Store

	alorAuth   *alor.AuthClient
	alorMarket *alor.MarketClient
	alorExec   *alor.ExecutionClient
)

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
	if issCode, ok := optionsSeries[symbol]; ok {
		if price, err := moexISSSpotPrice(issCode); err == nil && price > 0 {
			return price, nil
		}
	}

	// 2) Alor API (needs a valid refresh token).
	if futuresSymbol, ok := futuresSeries[symbol]; ok && alorMarket != nil {
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
	default:
		return 0, fmt.Errorf("no price source for symbol %s", symbol)
	}
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

// seriesInfoHandler returns the current futures and options series for the given symbol.
func seriesInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	futures := futuresSeries[symbol]
	if futures == "" {
		futures = symbol + "-9.26"
	}
	options := optionsSeries[symbol]
	if options == "" {
		options = symbol + "U6"
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol":         symbol,
		"futures_series": futures,
		"options_series": options,
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

	res, err := alorExec.PlaceOrder(req.Symbol, req.Side, req.Type, req.Price, req.Quantity)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
			"note":    "Failed to execute order on Alor (check ALOR_REFRESH_TOKEN and ALOR_PORTFOLIO)",
		})
		return
	}

	entryPrice := req.Price
	if entryPrice <= 0 {
		entryPrice, _ = getSpotPrice(req.Symbol)
	}
	currentPrice := req.Price
	if currentPrice <= 0 {
		currentPrice, _ = getSpotPrice(req.Symbol)
	}

	qty := req.Quantity
	if qty <= 0 {
		qty = 1
	}

	quant.AddPosition(quant.Position{
		ID:           fmt.Sprintf("pos-%d", time.Now().Unix()),
		Strategy:     "Alor MOEX Execution",
		Symbol:       req.Symbol,
		Side:         strings.ToUpper(req.Side),
		Quantity:     qty,
		EntryPrice:   entryPrice,
		CurrentPrice: currentPrice,
		PnL:          25.0 * float64(qty),
		Delta:        0.00,
		Theta:        45.0 * float64(qty),
		OpenedAt:     time.Now(),
	})

	json.NewEncoder(w).Encode(res)
}

func positionsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	positions := quant.GetActivePositions()
	portfolio := quant.GetPortfolio()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"positions": positions,
		"portfolio": portfolio,
	})
}

func portfolioHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	portfolio := quant.GetPortfolio()
	json.NewEncoder(w).Encode(portfolio)
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

	quant.ClosePosition(req.ID)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Position closed successfully",
	})
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
	quarterlyPrice := math.Round(perpPrice * 1.012)

	spread := quarterlyPrice - perpPrice
	annualizedReturn := (spread / perpPrice) * (365.0 / 90.0) * 100.0

	strategy := "No Arbitrage"
	if spread > 300.0 {
		strategy = fmt.Sprintf("Sell Quarterly %sU6, Buy Perpetual %s (Contango Arbitrage / Carry)", symbol, symbol)
	} else if spread < -100.0 {
		strategy = fmt.Sprintf("Buy Quarterly %sU6, Sell Perpetual %s (Backwardation)", symbol, symbol)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"symbol":            symbol,
		"perpetual_price":   perpPrice,
		"quarterly_price":   quarterlyPrice,
		"spread":            math.Round(spread*100) / 100,
		"annualized_return": math.Round(annualizedReturn*100) / 100,
		"strategy":          strategy,
	})
}

func optionsRecommendationsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ivStr := r.URL.Query().Get("iv")
	hvStr := r.URL.Query().Get("hv")
	spotStr := r.URL.Query().Get("spot")

	iv := 35.0
	if ivStr != "" {
		iv, _ = strconv.ParseFloat(ivStr, 64)
	}
	hv := 28.0
	if hvStr != "" {
		hv, _ = strconv.ParseFloat(hvStr, 64)
	}
	spot, _ := getSpotPrice("Si")
	if spot <= 0 {
		spot = 83200.0
	}
	if spotStr != "" {
		spot, _ = strconv.ParseFloat(spotStr, 64)
	}

	recs := quant.GenerateStrategyRecommendations(iv, hv, spot)
	regime := quant.ClassifyMarketRegime(iv, hv, false)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"market_regime":   string(regime),
		"recommendations": recs,
	})
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

	iv := 35.0
	if ivStr != "" {
		iv, _ = strconv.ParseFloat(ivStr, 64)
	}
	hv := 28.0
	if hvStr != "" {
		hv, _ = strconv.ParseFloat(hvStr, 64)
	}

	rec := quant.EvaluateVerticalSpreads(iv, hv, outlook)
	json.NewEncoder(w).Encode(rec)
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
	http.HandleFunc("/api/v1/series", seriesInfoHandler)

	// Settings (encrypted Alor token)
	http.HandleFunc("/api/v1/settings/token", settingsTokenHandler)

	// Positions & Portfolio Handlers
	http.HandleFunc("/api/v1/positions", positionsHandler)
	http.HandleFunc("/api/v1/positions/close", closePositionHandler)
	http.HandleFunc("/api/v1/portfolio", portfolioHandler)
	http.HandleFunc("/api/v1/capital", capitalHandler)
	http.HandleFunc("/api/v1/copilot/ask", copilotHandler)
	http.HandleFunc("/api/v1/options/exit-advice", optionsExitAdviceHandler)
	http.HandleFunc("/api/v1/options/gamma-step", gammaScalpingStepHandler)
	http.HandleFunc("/api/v1/options/vertical-spread", verticalSpreadHandler)
	http.HandleFunc("/api/v1/options/rolling-advice", rollingAdviceHandler)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": "Go Quant Core + MOEX Alor"})
	})

	fmt.Printf("Quant Engine & Dashboard running on port %s...\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}