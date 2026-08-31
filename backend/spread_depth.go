package main

// Depth-of-market for every option leg of an open spread. The profile view
// shows two full exchange order books (5 bids + 5 asks each) side by side,
// sourced live from Alor so the trader sees real resting liquidity before
// rolling / closing / hedging.

import (
	"encoding/json"
	"net/http"

	"option-quant-ai/quant"
)

const depthLevels = 5

type depthLevel struct {
	Price  float64 `json:"price"`
	Volume int     `json:"volume"`
}

type spreadLegDepth struct {
	SecID string       `json:"secid"`
	Side  string       `json:"side"`
	Bids  []depthLevel `json:"bids"`
	Asks  []depthLevel `json:"asks"`
	Src   string       `json:"src"`
}

type spreadDepthResponse struct {
	Legs []spreadLegDepth `json:"legs"`
	Src  string           `json:"src"`
}

// spreadDepthHandler returns the top 5 bid/ask levels of every option leg of
// an open spread, fetched from Alor.
// URL: GET /api/v1/spreads/depth?id=spr-...
func spreadDepthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		json.NewEncoder(w).Encode(map[string]string{"error": "id is required"})
		return
	}

	rec, ok := spreadRecordByID(id)
	if !ok {
		json.NewEncoder(w).Encode(map[string]string{"error": "spread not found"})
		return
	}

	// Locate the linked position and collect its option legs.
	var optLegs []struct {
		SecID string
		Side  string
	}
	for _, p := range quant.GetActivePositions() {
		if p.ID != rec.PositionID {
			continue
		}
		for _, l := range p.Legs {
			if l.Kind == "OPTION" && l.SecID != "" {
				optLegs = append(optLegs, struct {
					SecID string
					Side  string
				}{l.SecID, l.Side})
			}
		}
		break
	}

	resp := spreadDepthResponse{Legs: []spreadLegDepth{}, Src: ""}
	if alorMarket == nil {
		json.NewEncoder(w).Encode(resp)
		return
	}

	for _, ol := range optLegs {
		leg := spreadLegDepth{SecID: ol.SecID, Side: ol.Side}
		ob, err := alorMarket.FetchOrderbook("MOEX", ol.SecID)
		if err != nil {
			continue
		}
		for i := 0; i < len(ob.Bids) && i < depthLevels; i++ {
			leg.Bids = append(leg.Bids, depthLevel{Price: ob.Bids[i].Price, Volume: ob.Bids[i].Volume})
		}
		for i := 0; i < len(ob.Asks) && i < depthLevels; i++ {
			leg.Asks = append(leg.Asks, depthLevel{Price: ob.Asks[i].Price, Volume: ob.Asks[i].Volume})
		}
		resp.Src = "alor"
		resp.Legs = append(resp.Legs, leg)
	}

	json.NewEncoder(w).Encode(resp)
}
