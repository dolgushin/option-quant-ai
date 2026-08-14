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
	"time"

	"option-quant-ai/agent"
	"option-quant-ai/alor"
	"option-quant-ai/quant"
)

//go:embed static/*
var staticFiles embed.FS

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

func getSpotPrice(symbol string) (float64, error) {
	if symbol == "BTC" || symbol == "ETH" {
		q, err := quant.FetchCryptoSpot(symbol)
		if err != nil {
			return 92500.0, err
		}
		return q.Price, nil
	}
	switch symbol {
	case "Si":
		return 92500.0, nil
	case "RI":
		return 112000.0, nil
	case "CR":
		return 12.50, nil
	default:
		return 92500.0, nil
	}
}

func quoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si"
	}

	price, err := getSpotPrice(symbol)
	if err != nil {
		price = 92500.0
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
		spotPrice = 92500.0
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
		spotPrice = 92500.0
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
		spot = 92500.0
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

var (
	alorAuth   = alor.NewAuthClient(os.Getenv("ALOR_REFRESH_TOKEN"))
	alorMarket = alor.NewMarketClient(alorAuth)
	alorExec   = alor.NewExecutionClient(alorAuth, os.Getenv("ALOR_PORTFOLIO"))
)

func moexQuoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "Si-3.25"
	}

	quote, err := alorMarket.FetchSecurityQuote(symbol)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"symbol":   symbol,
			"exchange": "MOEX",
			"price":    92500.0,
			"bid":      92490.0,
			"ask":      92510.0,
			"note":     "Mock fallback (ALOR_REFRESH_TOKEN not configured): " + err.Error(),
		})
		return
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

	spot := 92500.0
	strike := 93000.0
	if strikeStr != "" {
		strike, _ = strconv.ParseFloat(strikeStr, 64)
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
		entryPrice = 92500.0
	}
	currentPrice := req.Price
	if currentPrice <= 0 {
		currentPrice = 92520.0
	}

	quant.AddPosition(quant.Position{
		ID:           fmt.Sprintf("pos-%d", time.Now().Unix()),
		Strategy:     "Alor MOEX Execution",
		Symbol:       req.Symbol,
		Side:         strings.ToUpper(req.Side),
		Quantity:     req.Quantity,
		EntryPrice:   entryPrice,
		CurrentPrice: currentPrice,
		PnL:          250.0,
		Delta:        0.00,
		Theta:        450.0,
		OpenedAt:     time.Now(),
	})

	json.NewEncoder(w).Encode(res)
}

func positionsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	positions := quant.GetActivePositions()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"positions": positions,
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

	perpPrice := 92500.0
	quarterlyPrice := 93200.0
	if symbol == "RI" {
		perpPrice = 112000.0
		quarterlyPrice = 113500.0
	} else if symbol == "CR" {
		perpPrice = 12.50
		quarterlyPrice = 12.85
	}

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
	spot := 92500.0
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

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

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

	// MOEX / Alor Handlers
	http.HandleFunc("/api/v1/moex/quote", moexQuoteHandler)
	http.HandleFunc("/api/v1/moex/arbitrage", moexArbitrageHandler)
	http.HandleFunc("/api/v1/moex/order", moexOrderHandler)
	http.HandleFunc("/api/v1/moex/perp-quarterly", moexPerpQuarterlyHandler)

	// Positions & Portfolio Handlers
	http.HandleFunc("/api/v1/positions", positionsHandler)
	http.HandleFunc("/api/v1/positions/close", closePositionHandler)
	http.HandleFunc("/api/v1/options/exit-advice", optionsExitAdviceHandler)
	http.HandleFunc("/api/v1/options/gamma-step", gammaScalpingStepHandler)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": "Go Quant Core + MOEX Alor"})
	})

	fmt.Printf("Quant Engine & Dashboard running on port %s...\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}