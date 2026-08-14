package quant

import (
	"sync"
	"time"
)

type Position struct {
	ID        string    `json:"id"`
	Strategy  string    `json:"strategy"`
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"` // "BUY" or "SELL"
	Quantity  int       `json:"quantity"`
	EntryPrice float64  `json:"entry_price"`
	CurrentPrice float64 `json:"current_price"`
	PnL       float64   `json:"pnl"`
	Delta     float64   `json:"delta"`
	Theta     float64   `json:"theta"`
	OpenedAt  time.Time `json:"opened_at"`
}

type PortfolioState struct {
	InitialCapital float64 `json:"initial_capital"`
	Cash           float64 `json:"cash"`
	LockedMargin   float64 `json:"locked_margin"`
	TotalValue     float64 `json:"total_value"`
}

var (
	positionsMu    sync.Mutex
	initialCapital = 1000000.0
	lockedMargin   = 15000.0
	activePositions = []Position{
		{
			ID:           "pos-001",
			Strategy:     "Basis Arbitrage (SiU6 vs Si)",
			Symbol:       "SiU6 / Si",
			Side:         "SPREAD",
			Quantity:     10,
			EntryPrice:   700.0,
			CurrentPrice: 720.0,
			PnL:          200.0,
			Delta:        0.00,
			Theta:        120.0,
			OpenedAt:     time.Now().Add(-2 * time.Hour),
		},
	}
)

func GetPortfolio() PortfolioState {
	positionsMu.Lock()
	defer positionsMu.Unlock()

	totalPnL := 0.0
	for _, p := range activePositions {
		totalPnL += p.PnL
	}

	cash := initialCapital - lockedMargin + totalPnL
	totalValue := initialCapital + totalPnL

	return PortfolioState{
		InitialCapital: initialCapital,
		Cash:           cash,
		LockedMargin:   lockedMargin,
		TotalValue:     totalValue,
	}
}

func SetInitialCapital(amount float64) {
	positionsMu.Lock()
	defer positionsMu.Unlock()
	initialCapital = amount
}

func GetActivePositions() []Position {
	positionsMu.Lock()
	defer positionsMu.Unlock()
	return activePositions
}

func AddPosition(p Position) {
	positionsMu.Lock()
	defer positionsMu.Unlock()
	activePositions = append(activePositions, p)
}

func ClosePosition(id string) {
	positionsMu.Lock()
	defer positionsMu.Unlock()
	var filtered []Position
	for _, p := range activePositions {
		if p.ID != id {
			filtered = append(filtered, p)
		}
	}
	activePositions = filtered
}
