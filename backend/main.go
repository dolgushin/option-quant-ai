package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"option-quant-ai/quant"
)

type GreeksRequest struct {
	IsCall bool    `json:"is_call"`
	S      float64 `json:"spot_price"`  // Текущая цена актива (Spot)
	K      float64 `json:"strike_price"`// Страйк опциона
	T      float64 `json:"time_to_exp"` // Время до экспирации в годах (напр. 30/365)
	R      float64 `json:"risk_free"`   // Безрисковая ставка (напр. 0.05)
	Sigma  float64 `json:"volatility"`  // Волатильность (напр. 0.20 для 20%)
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

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	http.HandleFunc("/api/v1/greeks", greeksHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": "Go Quant Core"})
	})

	fmt.Printf("Quant Engine starting on port %s...\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}