package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"option-quant-ai/quant"
)

type GreeksRequest struct {
	IsCall bool    `json:"is_call"`
	S      float64 `json:"spot_price"`
	K      float64 `json:"strike_price"`
	T      float64 `json:"time_to_exp"`
	R      float64 `json:"risk_free"`
	Sigma  float64 `json:"volatility"`
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

func quoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "BTC"
	}

	quote, err := quant.FetchCryptoSpot(symbol)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(quote)
}

// liveGreeksHandler получает спот из Deribit и сразу считает Греки
func liveGreeksHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		symbol = "BTC"
	}

	strikeStr := r.URL.Query().Get("strike")
	daysStr := r.URL.Query().Get("days")
	volStr := r.URL.Query().Get("vol")
	isCallStr := r.URL.Query().Get("is_call")

	quote, err := quant.FetchCryptoSpot(symbol)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "Failed to fetch spot price: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	strike, _ := strconv.ParseFloat(strikeStr, 64)
	if strike == 0 {
		strike = quote.Price // По умолчанию ATM (At-The-Money)
	}

	days, _ := strconv.ParseFloat(daysStr, 64)
	if days == 0 {
		days = 30 // 30 дней по умолчанию
	}

	vol, _ := strconv.ParseFloat(volStr, 64)
	if vol == 0 {
		vol = 0.50 // 50% IV по умолчанию для крипты
	}

	isCall := isCallStr != "false"

	t := days / 365.0
	rRate := 0.05

	greeks := quant.CalculateBlackScholes(isCall, quote.Price, strike, t, rRate, vol)

	response := map[string]interface{}{
		"symbol":     symbol,
		"spot_price": quote.Price,
		"strike":     strike,
		"days_to_exp": days,
		"volatility": vol,
		"is_call":    isCall,
		"greeks":     greeks,
	}

	json.NewEncoder(w).Encode(response)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	http.HandleFunc("/api/v1/greeks", greeksHandler)
	http.HandleFunc("/api/v1/market/quote", quoteHandler)
	http.HandleFunc("/api/v1/greeks/live", liveGreeksHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": "Go Quant Core"})
	})

	fmt.Printf("Quant Engine starting on port %s...\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}