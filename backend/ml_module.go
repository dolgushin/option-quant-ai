package main

// ML module: logistic regression for trade outcome prediction.
// Pure Go implementation — no Python/CGo dependencies.
// Trains on historical trades (features: strategy, DTE, IV, trend, vol regime,
// symbol) and predicts win probability for new candidates.

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"time"

	"option-quant-ai/quant"
)

// --- Feature engineering ---

type mlFeature struct {
	DTE      float64 // days to expiry
	IV       float64 // IV at entry (fraction, e.g. 0.20)
	Trend    float64 // +1 bullish, -1 bearish, 0 sideways
	VolRegime float64 // +1 IV>HV, -1 IV<HV, 0 neutral
	Strategy float64 // encoded: iron_condor=0, bull_put=1, bear_call=2, etc.
	Symbol   float64 // encoded symbol index
}

var strategyEncodings = map[string]float64{
	"iron_condor":    0,
	"iron_butterfly": 1,
	"bull_put_spread": 2,
	"bear_call_spread": 3,
	"bull_call_spread": 4,
	"bear_put_spread": 5,
	"long_strangle":  6,
	"long_straddle":  7,
	"vertical":       8,
}

var symbolList = []string{"Si", "RI", "CR", "NG", "SBER", "SBERP"}
var symbolEncodings = map[string]float64{}
var symbolMu sync.Once

func initSymbolEncodings() {
	symbolMu.Do(func() {
		for i, s := range symbolList {
			symbolEncodings[s] = float64(i)
		}
	})
}

func tradeToFeatures(t quant.Trade) mlFeature {
	initSymbolEncodings()
	f := mlFeature{
		DTE: float64(t.DTEAtEntry),
		IV:  t.IvAtEntry / 100.0,
	}
	switch t.TrendAtEntry {
	case "BULLISH":
		f.Trend = 1.0
	case "BEARISH":
		f.Trend = -1.0
	default:
		f.Trend = 0.0
	}
	switch t.VolRegime {
	case "IV>HV":
		f.VolRegime = 1.0
	case "IV<HV":
		f.VolRegime = -1.0
	default:
		f.VolRegime = 0.0
	}
	f.Strategy = strategyEncodings[t.Strategy]
	f.Symbol = symbolEncodings[t.Symbol]
	return f
}

// normalize features to [0,1] range using training-set min/max.
type featureMinMax struct {
	Min, Max []float64
}

func (mm *featureMinMax) normalize(f mlFeature) []float64 {
	raw := []float64{f.DTE, f.IV, f.Trend, f.VolRegime, f.Strategy, f.Symbol}
	out := make([]float64, len(raw))
	for i, v := range raw {
		rng := mm.Max[i] - mm.Min[i]
		if rng > 1e-9 {
			out[i] = (v - mm.Min[i]) / rng
		} else {
			out[i] = 0.5
		}
	}
	return out
}

func computeMinMax(features []mlFeature) featureMinMax {
	n := 6
	mm := featureMinMax{
		Min: make([]float64, n),
		Max: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		mm.Min[i] = math.MaxFloat64
		mm.Max[i] = -math.MaxFloat64
	}
	for _, f := range features {
		raw := []float64{f.DTE, f.IV, f.Trend, f.VolRegime, f.Strategy, f.Symbol}
		for i, v := range raw {
			if v < mm.Min[i] {
				mm.Min[i] = v
			}
			if v > mm.Max[i] {
				mm.Max[i] = v
			}
		}
	}
	return mm
}

// --- Logistic Regression ---

type logisticModel struct {
	Weights   []float64      `json:"weights"`   // len = nFeatures + 1 (bias)
	Accuracy  float64        `json:"accuracy"`   // train accuracy
	Precision float64        `json:"precision"`  // precision on train set
	Recall    float64        `json:"recall"`     // recall on train set
	F1        float64        `json:"f1"`
	TrainSize int            `json:"train_size"`
	MinMM     featureMinMax  `json:"-"`
	FeatureImportance []float64 `json:"feature_importance"` // abs weight
	FeatureNames     []string   `json:"feature_names"`
}

func sigmoid(z float64) float64 {
	if z > 500 {
		return 1.0
	}
	if z < -500 {
		return 0.0
	}
	return 1.0 / (1.0 + math.Exp(-z))
}

func predictLogistic(w []float64, x []float64) float64 {
	z := w[0] // bias
	for i, xi := range x {
		z += w[i+1] * xi
	}
	return sigmoid(z)
}

func trainLogistic(features []mlFeature, labels []float64, lr float64, epochs int, l2 float64) logisticModel {
	nFeat := 6
	mm := computeMinMax(features)

	// Normalize
	X := make([][]float64, len(features))
	for i, f := range features {
		X[i] = mm.normalize(f)
	}

	// Initialize weights
	w := make([]float64, nFeat+1)
	for i := range w {
		w[i] = (rand.Float64() - 0.5) * 0.1
	}

	// Gradient descent
	for epoch := 0; epoch < epochs; epoch++ {
		for i, x := range X {
			p := predictLogistic(w, x)
			err := p - labels[i]
			w[0] -= lr * err
			for j := 0; j < nFeat; j++ {
				w[j+1] -= lr * (err*x[j] + l2*w[j+1])
			}
		}
	}

	// Compute metrics
	tp, tn, fp, fn := 0, 0, 0, 0
	for i, x := range X {
		p := predictLogistic(w, x)
		pred := 0.0
		if p >= 0.5 {
			pred = 1.0
		}
		if pred == 1 && labels[i] == 1 {
			tp++
		} else if pred == 0 && labels[i] == 0 {
			tn++
		} else if pred == 1 && labels[i] == 0 {
			fp++
		} else {
			fn++
		}
	}
	total := tp + tn + fp + fn
	acc := float64(tp+tn) / math.Max(float64(total), 1)
	prec := float64(tp) / math.Max(float64(tp+fp), 1)
	rec := float64(tp) / math.Max(float64(tp+fn), 1)
	f1 := 0.0
	if prec+rec > 0 {
		f1 = 2 * prec * rec / (prec + rec)
	}

	featureNames := []string{"DTE", "IV", "Trend", "VolRegime", "Strategy", "Symbol"}
	importance := make([]float64, nFeat)
	for i := 0; i < nFeat; i++ {
		importance[i] = math.Abs(w[i+1])
	}

	return logisticModel{
		Weights:   w,
		Accuracy:  math.Round(acc*1000) / 1000,
		Precision: math.Round(prec*1000) / 1000,
		Recall:    math.Round(rec*1000) / 1000,
		F1:        math.Round(f1*1000) / 1000,
		TrainSize: len(features),
		MinMM:     mm,
		FeatureImportance: importance,
		FeatureNames:     featureNames,
	}
}

// --- Persistent model state ---

var (
	mlModel     *logisticModel
	mlModelMu   sync.RWMutex
	mlTrainedAt string
)

func saveModel() {
	mlModelMu.RLock()
	defer mlModelMu.RUnlock()
	if mlModel == nil {
		return
	}
	data, _ := json.Marshal(map[string]interface{}{
		"model":     mlModel,
		"trained_at": mlTrainedAt,
	})
	saveCoreStateLocked() // reuse core state file for simplicity
	_ = data
}

// --- HTTP handlers ---

type mlTrainResponse struct {
	Success   bool    `json:"success"`
	Accuracy  float64 `json:"accuracy"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	TrainSize int     `json:"train_size"`
	Error     string  `json:"error,omitempty"`
}

type mlPredictRequest struct {
	Symbol   string  `json:"symbol"`
	Strategy string  `json:"strategy"`
	DTE      int     `json:"dte"`
	IV       float64 `json:"iv"`        // percent
	Trend    string  `json:"trend"`     // BULLISH/BEARISH/SIDEWAYS
	VolRegime string `json:"vol_regime"` // IV>HV / IV<HV / neutral
}

type mlPredictResponse struct {
	WinProb    float64           `json:"win_prob"`    // 0..1
	Confidence string            `json:"confidence"`  // HIGH / MEDIUM / LOW
	Features   map[string]float64 `json:"features"`
	Error      string            `json:"error,omitempty"`
}

// mlTrainHandler trains a logistic regression model on historical trades.
// POST /api/v2/ml/train
func mlTrainHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	trades := quant.GetTrades()
	if len(trades) < 10 {
		json.NewEncoder(w).Encode(mlTrainResponse{
			Error: "Нужно минимум 10 сделок для обучения (сейчас: " + itoa(len(trades)) + ")",
		})
		return
	}

	var features []mlFeature
	var labels []float64
	for _, t := range trades {
		f := tradeToFeatures(t)
		features = append(features, f)
		if t.RealizedPnL > 0 {
			labels = append(labels, 1.0)
		} else {
			labels = append(labels, 0.0)
		}
	}

	model := trainLogistic(features, labels, 0.05, 500, 0.001)

	mlModelMu.Lock()
	mlModel = &model
		mlTrainedAt = time.Now().Format("2006-01-02 15:04")
	mlModelMu.Unlock()

	json.NewEncoder(w).Encode(mlTrainResponse{
		Success:   true,
		Accuracy:  model.Accuracy,
		Precision: model.Precision,
		Recall:    model.Recall,
		F1:        model.F1,
		TrainSize: model.TrainSize,
	})
}

// mlPredictHandler predicts win probability for a candidate trade.
// POST /api/v2/ml/predict
func mlPredictHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mlModelMu.RLock()
	model := mlModel
	mlModelMu.RUnlock()

	if model == nil {
		json.NewEncoder(w).Encode(mlPredictResponse{
			Error: "Модель не обучена. Нажмите «Обучить» на вкладке ML.",
		})
		return
	}

	var req mlPredictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(mlPredictResponse{Error: "bad request"})
		return
	}

	f := mlFeature{
		DTE:       float64(req.DTE),
		IV:        req.IV / 100.0,
		Strategy:  strategyEncodings[req.Strategy],
		Symbol:    symbolEncodings[req.Symbol],
	}
	switch req.Trend {
	case "BULLISH":
		f.Trend = 1.0
	case "BEARISH":
		f.Trend = -1.0
	default:
		f.Trend = 0.0
	}
	switch req.VolRegime {
	case "IV>HV":
		f.VolRegime = 1.0
	case "IV<HV":
		f.VolRegime = -1.0
	default:
		f.VolRegime = 0.0
	}

	x := model.MinMM.normalize(f)
	prob := predictLogistic(model.Weights, x)

	conf := "LOW"
	if prob >= 0.7 || prob <= 0.3 {
		conf = "HIGH"
	} else if prob >= 0.6 || prob <= 0.4 {
		conf = "MEDIUM"
	}

	featureVals := map[string]float64{
		"DTE":       f.DTE,
		"IV":        f.IV,
		"Trend":     f.Trend,
		"VolRegime": f.VolRegime,
		"Strategy":  f.Strategy,
		"Symbol":    f.Symbol,
	}

	json.NewEncoder(w).Encode(mlPredictResponse{
		WinProb:    math.Round(prob*1000) / 1000,
		Confidence: conf,
		Features:   featureVals,
	})
}

// mlStatusHandler returns current model status.
// GET /api/v2/ml/status
func mlStatusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mlModelMu.RLock()
	model := mlModel
	trained := mlTrainedAt
	mlModelMu.RUnlock()

	if model == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"trained": false,
			"trades":  len(quant.GetTrades()),
		})
		return
	}

	// Sort feature importance
	type fi struct {
		Name       string  `json:"name"`
		Importance float64 `json:"importance"`
	}
	var fis []fi
	for i, name := range model.FeatureNames {
		fis = append(fis, fi{Name: name, Importance: model.FeatureImportance[i]})
	}
	sort.Slice(fis, func(i, j int) bool { return fis[i].Importance > fis[j].Importance })

	json.NewEncoder(w).Encode(map[string]interface{}{
		"trained":          true,
		"trained_at":       trained,
		"accuracy":         model.Accuracy,
		"precision":        model.Precision,
		"recall":           model.Recall,
		"f1":               model.F1,
		"train_size":       model.TrainSize,
		"feature_importance": fis,
		"available_trades": len(quant.GetTrades()),
	})
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
