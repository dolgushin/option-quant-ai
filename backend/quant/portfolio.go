package quant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PositionLeg is a single leg (option or futures) of a strategy position.
type PositionLeg struct {
	SecID        string  `json:"secid"`
	Symbol       string  `json:"symbol"`
	Kind         string  `json:"kind"` // "OPTION" or "FUTURES"
	Side         string  `json:"side"` // "BUY" or "SELL"
	Quantity     int     `json:"quantity"`
	Strike       float64 `json:"strike"`
	IsCall       bool    `json:"is_call"`
	EntryPrice   float64 `json:"entry_price"`
	CurrentPrice float64 `json:"current_price"`
}

// Position is a multi-leg strategy position.
type Position struct {
	ID        string        `json:"id"`
	Strategy  string        `json:"strategy"`
	Symbol    string        `json:"symbol"`
	Expiry    string        `json:"expiry"`
	Legs      []PositionLeg `json:"legs"`
	OpenedAt  time.Time     `json:"opened_at"`
	Delta     float64       `json:"delta"`
	Theta     float64       `json:"theta"`
	Margin    float64       `json:"margin"`
	EntryValue float64      `json:"entry_value"`
	CurrentValue float64    `json:"current_value"`
	PnL       float64       `json:"pnl"`
	PnLPercent float64      `json:"pnl_percent"`
}

// Trade is a closed position with realized PnL.
type Trade struct {
	ID          string    `json:"id"`
	Strategy    string    `json:"strategy"`
	Symbol      string    `json:"symbol"`
	OpenedAt    time.Time `json:"opened_at"`
	ClosedAt    time.Time `json:"closed_at"`
	EntryValue  float64   `json:"entry_value"`
	ExitValue   float64   `json:"exit_value"`
	RealizedPnL float64   `json:"realized_pnl"`
	PnLPercent  float64   `json:"pnl_percent"`
}

// Stats aggregates closed-trade statistics.
type Stats struct {
	TotalTrades       int     `json:"total_trades"`
	WinningTrades     int     `json:"winning_trades"`
	LosingTrades      int     `json:"losing_trades"`
	WinRate           float64 `json:"win_rate"`
	TotalRealizedPnL  float64 `json:"total_realized_pnl"`
	TotalUnrealizedPnL float64 `json:"total_unrealized_pnl"`
	AvgWin            float64 `json:"avg_win"`
	AvgLoss           float64 `json:"avg_loss"`
	ProfitFactor      float64 `json:"profit_factor"`
	BestTrade         float64 `json:"best_trade"`
	WorstTrade        float64 `json:"worst_trade"`
}

type PortfolioState struct {
	InitialCapital float64 `json:"initial_capital"`
	Cash           float64 `json:"cash"`
	LockedMargin   float64 `json:"locked_margin"`
	UnrealizedPnL  float64 `json:"unrealized_pnl"`
	TotalValue     float64 `json:"total_value"`
}

var (
	positionsMu    sync.Mutex
	initialCapital = 1000000.0
	activePositions []Position
	tradeHistory   []Trade
	dataFile       string
)

// SetDataFile sets the JSON file used for persistence (absolute path).
func SetDataFile(path string) {
	positionsMu.Lock()
	defer positionsMu.Unlock()
	dataFile = path
}

// Load reads positions, trades and capital from the persisted JSON file.
func Load() {
	positionsMu.Lock()
	defer positionsMu.Unlock()
	if dataFile == "" {
		return
	}
	b, err := os.ReadFile(dataFile)
	if err != nil {
		return
	}
	var state struct {
		InitialCapital float64   `json:"initial_capital"`
		Positions      []Position `json:"positions"`
		Trades         []Trade    `json:"trades"`
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return
	}
	if state.InitialCapital > 0 {
		initialCapital = state.InitialCapital
	}
	if state.Positions != nil {
		activePositions = state.Positions
	}
	if state.Trades != nil {
		tradeHistory = state.Trades
	}
}

// Persist writes the current state to disk. Callers should not hold positionsMu.
func Persist() {
	positionsMu.Lock()
	state := struct {
		InitialCapital float64    `json:"initial_capital"`
		Positions      []Position `json:"positions"`
		Trades         []Trade    `json:"trades"`
	}{
		InitialCapital: initialCapital,
		Positions:      activePositions,
		Trades:         tradeHistory,
	}
	positionsMu.Unlock()

	if dataFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dataFile), 0755); err != nil {
		return
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(dataFile, b, 0644)
}

func GetPortfolio() PortfolioState {
	positionsMu.Lock()
	defer positionsMu.Unlock()

	totalPnL := 0.0
	currentLockedMargin := 0.0
	for _, p := range activePositions {
		totalPnL += p.PnL
		currentLockedMargin += p.Margin
	}

	cash := initialCapital - currentLockedMargin + totalPnL
	totalValue := initialCapital + totalPnL

	return PortfolioState{
		InitialCapital: initialCapital,
		Cash:           cash,
		LockedMargin:   currentLockedMargin,
		UnrealizedPnL:  totalPnL,
		TotalValue:     totalValue,
	}
}

func SetInitialCapital(amount float64) {
	positionsMu.Lock()
	initialCapital = amount
	positionsMu.Unlock()
	Persist()
}

func GetActivePositions() []Position {
	positionsMu.Lock()
	defer positionsMu.Unlock()
	return activePositions
}

func SetPositions(positions []Position) {
	positionsMu.Lock()
	activePositions = positions
	positionsMu.Unlock()
	Persist()
}

// SavePosition persists a single position (add or replace by ID).
func SavePosition(p Position) {
	positionsMu.Lock()
	found := false
	for i := range activePositions {
		if activePositions[i].ID == p.ID {
			activePositions[i] = p
			found = true
			break
		}
	}
	if !found {
		activePositions = append(activePositions, p)
	}
	positionsMu.Unlock()
	Persist()
}

// RemovePosition deletes a position and returns it (for closing).
func RemovePosition(id string) (Position, bool) {
	positionsMu.Lock()
	var removed Position
	found := false
	filtered := activePositions[:0]
	for _, p := range activePositions {
		if p.ID == id {
			removed = p
			found = true
			continue
		}
		filtered = append(filtered, p)
	}
	activePositions = filtered
	positionsMu.Unlock()
	if found {
		Persist()
	}
	return removed, found
}

func GetTrades() []Trade {
	positionsMu.Lock()
	defer positionsMu.Unlock()
	return tradeHistory
}

// AddTrade records a closed trade and persists it.
func AddTrade(t Trade) {
	positionsMu.Lock()
	tradeHistory = append(tradeHistory, t)
	positionsMu.Unlock()
	Persist()
}

// ComputeStats aggregates statistics over all closed trades.
func ComputeStats() Stats {
	positionsMu.Lock()
	defer positionsMu.Unlock()

	var s Stats
	s.TotalTrades = len(tradeHistory)
	var wins, losses, winTotal, lossTotal float64
	for _, t := range tradeHistory {
		if t.RealizedPnL > 0 {
			wins++
			winTotal += t.RealizedPnL
			if s.BestTrade == 0 || t.RealizedPnL > s.BestTrade {
				s.BestTrade = t.RealizedPnL
			}
		} else if t.RealizedPnL < 0 {
			losses++
			lossTotal += t.RealizedPnL
			if s.WorstTrade == 0 || t.RealizedPnL < s.WorstTrade {
				s.WorstTrade = t.RealizedPnL
			}
		}
		s.TotalRealizedPnL += t.RealizedPnL
	}
	s.WinningTrades = int(wins)
	s.LosingTrades = int(losses)
	if s.TotalTrades > 0 {
		s.WinRate = wins / float64(s.TotalTrades) * 100
	}
	if wins > 0 {
		s.AvgWin = winTotal / wins
	}
	if losses > 0 {
		s.AvgLoss = lossTotal / losses
	}
	if lossTotal != 0 {
		s.ProfitFactor = -winTotal / lossTotal
	} else if winTotal > 0 {
		s.ProfitFactor = 99999.0 // no losses → infinite profit factor
	}
	for _, p := range activePositions {
		s.TotalUnrealizedPnL += p.PnL
	}
	return s
}
