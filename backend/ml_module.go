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
	"strings"
	"sync"
	"time"

	"option-quant-ai/quant"
)

// --- Feature engineering ---

type mlFeature struct {
	DTE       float64 // days to expiry
	IV        float64 // IV at entry (fraction, e.g. 0.20)
	Trend     float64 // +1 bullish, -1 bearish, 0 sideways
	VolRegime float64 // +1 IV>HV, -1 IV<HV, 0 neutral
	Strategy  float64 // encoded: iron_condor=0, bull_put=1, bear_call=2, etc.
	Symbol    float64 // encoded symbol index
}

var strategyEncodings = map[string]float64{
	"iron_condor":    0,
	"iron_butterfly": 1,
	"bull_put":       2,
	"bear_call":      3,
	"bull_call":      4,
	"bear_put":       5,
	"long_strangle":  6,
	"long_straddle":  7,
	"vertical":       8,
}

// encodeStrategyName normalizes strategy names from all sources — trade
// journal display names ("Bull Put Spread"), API keys ("bull_put_spread"),
// short codes ("IC") — to the canonical encoding. Unknown names yield -1
// (distinct from every known class) instead of silently collapsing to
// iron_condor=0, which used to flatten the whole strategy axis in training.
func encodeStrategyName(s string) (float64, bool) {
	norm := strings.ToLower(strings.TrimSpace(s))
	norm = strings.ReplaceAll(norm, " ", "_")
	norm = strings.TrimSuffix(norm, "_spread")
	if v, ok := strategyEncodings[norm]; ok {
		return v, true
	}
	if norm == "ic" {
		return strategyEncodings["iron_condor"], true
	}
	return -1, false
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
	f.Strategy, _ = encodeStrategyName(t.Strategy)
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
		// Clamp: inputs outside the training range (e.g. DTE 100 when the
		// model saw ≤45) must not explode the dot product — score them as
		// the nearest seen edge instead of extrapolating to 99%.
		if out[i] < 0 {
			out[i] = 0
		}
		if out[i] > 1 {
			out[i] = 1
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
	Weights           []float64     `json:"weights"`   // len = nFeatures + 1 (bias)
	Accuracy          float64       `json:"accuracy"`  // train accuracy
	Precision         float64       `json:"precision"` // precision on train set
	Recall            float64       `json:"recall"`    // recall on train set
	F1                float64       `json:"f1"`
	TrainSize         int           `json:"train_size"`
	MinMM             featureMinMax `json:"-"`
	FeatureImportance []float64     `json:"feature_importance"` // abs weight
	FeatureNames      []string      `json:"feature_names"`
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
		Weights:           w,
		Accuracy:          math.Round(acc*1000) / 1000,
		Precision:         math.Round(prec*1000) / 1000,
		Recall:            math.Round(rec*1000) / 1000,
		F1:                math.Round(f1*1000) / 1000,
		TrainSize:         len(features),
		MinMM:             mm,
		FeatureImportance: importance,
		FeatureNames:      featureNames,
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
		"model":      mlModel,
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
	Symbol    string  `json:"symbol"`
	Strategy  string  `json:"strategy"`
	DTE       int     `json:"dte"`
	IV        float64 `json:"iv"`         // percent
	Trend     string  `json:"trend"`      // BULLISH/BEARISH/SIDEWAYS
	VolRegime string  `json:"vol_regime"` // IV>HV / IV<HV / neutral
}

type mlPredictResponse struct {
	WinProb    float64            `json:"win_prob"`   // 0..1
	Confidence string             `json:"confidence"` // HIGH / MEDIUM / LOW
	Features   map[string]float64 `json:"features"`
	Error      string             `json:"error,omitempty"`
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

	stratCode, ok := encodeStrategyName(req.Strategy)
	if !ok {
		json.NewEncoder(w).Encode(mlPredictResponse{Error: "неизвестная стратегия: " + req.Strategy})
		return
	}
	f := mlFeature{
		DTE:      float64(req.DTE),
		IV:       req.IV / 100.0,
		Strategy: stratCode,
		Symbol:   symbolEncodings[req.Symbol],
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

	prob, conf := scoreMLFeature(model, f)

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

// scoreMLFeature runs the model on one feature vector: win probability plus
// the HIGH/MEDIUM/LOW confidence band shared by single predict and grid scan.
func scoreMLFeature(model *logisticModel, f mlFeature) (float64, string) {
	x := model.MinMM.normalize(f)
	prob := predictLogistic(model.Weights, x)
	conf := "LOW"
	if prob >= 0.7 || prob <= 0.3 {
		conf = "HIGH"
	} else if prob >= 0.6 || prob <= 0.4 {
		conf = "MEDIUM"
	}
	return prob, conf
}

// Default scan grid: Si/RI universe, six traded strategies; the DTE axis
// defaults to live exchange expiries (weeklies/monthlies/quarterlies),
// other axes are compact fixed grids.
var (
	defaultScanSymbols    = []string{"Si", "RI"}
	defaultScanStrategies = []string{"iron_condor", "bull_put_spread", "bear_call_spread", "bull_call_spread", "bear_put_spread", "long_strangle"}
	defaultScanDTEs       = []int{7, 14, 21, 30, 45}
	defaultScanIVs        = []float64{15, 20, 30, 45, 60}
	defaultScanTrends     = []string{"BULLISH", "BEARISH", "SIDEWAYS"}
	defaultScanVols       = []string{"IV>HV", "IV<HV", "neutral"}
)

// filterSeriesDTEs keeps tradeable tenors from a series date list: DTE
// within 1..120 days, deduplicated and sorted. Pure — unit-tested.
func filterSeriesDTEs(dates []string, today time.Time) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, d := range dates {
		dte := dteInDays(d, today)
		if dte < 1 || dte > 120 {
			continue
		}
		if !seen[dte] {
			seen[dte] = true
			out = append(out, dte)
		}
	}
	sort.Ints(out)
	return out
}

// liveSeriesDTEs resolves the DTE axis from real option expiries of the given
// symbols (union, sorted). Falls back to the fixed grid when the chain is
// unreachable. Returns the DTEs and the source label for the UI.
func liveSeriesDTEs(symbols []string) ([]int, string) {
	now := time.Now()
	seen := map[int]bool{}
	out := []int{}
	for _, sym := range symbols {
		for _, s := range optionSeriesForSymbol(sym) {
			dte := dteInDays(s.LastDelDate, now)
			if dte < 1 || dte > 120 {
				continue
			}
			if !seen[dte] {
				seen[dte] = true
				out = append(out, dte)
			}
		}
	}
	if len(out) == 0 {
		return defaultScanDTEs, "grid"
	}
	// No tenor truncation here (weeklies AND quarterlies must survive);
	// the handler's 20000-combo grid cap bounds the cost instead.
	sort.Ints(out)
	return out, "series"
}

type mlScanRequest struct {
	Symbols    []string  `json:"symbols"`
	Strategies []string  `json:"strategies"`
	DTEs       []int     `json:"dtes"`
	IVs        []float64 `json:"ivs"`
	Trends     []string  `json:"trends"`
	Vols       []string  `json:"vols"`
	Top        int       `json:"top"`
	PerTenor   int       `json:"per_tenor"`
}

type mlScanRow struct {
	Symbol     string  `json:"symbol"`
	Strategy   string  `json:"strategy"`
	DTE        int     `json:"dte"`
	IV         float64 `json:"iv"`
	Trend      string  `json:"trend"`
	VolRegime  string  `json:"vol_regime"`
	WinProb    float64 `json:"win_prob"`
	Confidence string  `json:"confidence"`
}

// tenorOf buckets a DTE into an expiry tenor for grouped output.
func tenorOf(dte int) string {
	switch {
	case dte <= 10:
		return "weekly"
	case dte <= 45:
		return "monthly"
	default:
		return "quarterly"
	}
}

func tenorLabel(t string) string {
	switch t {
	case "weekly":
		return "Недельные (≤10д)"
	case "monthly":
		return "Месячные (11–45д)"
	default:
		return "Квартальные (>45д)"
	}
}

type mlScanGroup struct {
	Tenor string      `json:"tenor"`
	Label string      `json:"label"`
	Rows  []mlScanRow `json:"rows"`
}

// scoreMLGrid scores every grid combination, sorted by win probability desc.
// Pure — the shared engine behind flat top-N and tenor-grouped output.
func scoreMLGrid(model *logisticModel, symbols, strategies []string, dtes []int, ivs []float64, trends, vols []string) []mlScanRow {
	rows := []mlScanRow{}
	for _, sym := range symbols {
		for _, strat := range strategies {
			stratCode, ok := encodeStrategyName(strat)
			if !ok {
				continue
			}
			for _, dte := range dtes {
				for _, iv := range ivs {
					for _, trend := range trends {
						var tr float64
						switch trend {
						case "BULLISH":
							tr = 1.0
						case "BEARISH":
							tr = -1.0
						}
						for _, vol := range vols {
							var vr float64
							switch vol {
							case "IV>HV":
								vr = 1.0
							case "IV<HV":
								vr = -1.0
							}
							f := mlFeature{
								DTE: float64(dte), IV: iv / 100.0,
								Trend: tr, VolRegime: vr,
								Strategy: stratCode, Symbol: symbolEncodings[sym],
							}
							prob, conf := scoreMLFeature(model, f)
							rows = append(rows, mlScanRow{
								Symbol: sym, Strategy: strat,
								DTE: dte, IV: iv, Trend: trend, VolRegime: vol,
								WinProb: math.Round(prob*1000) / 1000, Confidence: conf,
							})
						}
					}
				}
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].WinProb > rows[j].WinProb })
	return rows
}

// scanMLCombinations scores every grid combination with the model and returns
// the top-N rows by win probability. Pure — unit-tested.
func scanMLCombinations(model *logisticModel, symbols, strategies []string, dtes []int, ivs []float64, trends, vols []string, top int) []mlScanRow {
	rows := scoreMLGrid(model, symbols, strategies, dtes, ivs, trends, vols)
	if top > 0 && len(rows) > top {
		rows = rows[:top]
	}
	return rows
}

// scanMLGrouped returns the top-N rows per expiry tenor (weekly/monthly/
// quarterly) so a dominant axis (e.g. DTE) cannot crowd out every other
// expiry from the output. Buckets follow tenor order; empty ones are
// omitted. Pure — unit-tested.
func scanMLGrouped(model *logisticModel, symbols, strategies []string, dtes []int, ivs []float64, trends, vols []string, perTenor int) []mlScanGroup {
	rows := scoreMLGrid(model, symbols, strategies, dtes, ivs, trends, vols)
	byTenor := map[string][]mlScanRow{}
	order := []string{}
	for _, r := range rows {
		t := tenorOf(r.DTE)
		if _, ok := byTenor[t]; !ok {
			order = append(order, t)
		}
		byTenor[t] = append(byTenor[t], r)
	}
	// Tenor order: weekly, monthly, quarterly.
	rank := map[string]int{"weekly": 0, "monthly": 1, "quarterly": 2}
	sort.Slice(order, func(i, j int) bool { return rank[order[i]] < rank[order[j]] })
	groups := []mlScanGroup{}
	for _, t := range order {
		rs := byTenor[t]
		if perTenor > 0 && len(rs) > perTenor {
			rs = rs[:perTenor]
		}
		groups = append(groups, mlScanGroup{Tenor: t, Label: tenorLabel(t), Rows: rs})
	}
	return groups
}

// mlScanHandler scores a full parameter grid with the trained model and
// returns the best combinations — "перебрать всё и выдать лучшие".
// POST /api/v2/ml/scan
func mlScanHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mlModelMu.RLock()
	model := mlModel
	mlModelMu.RUnlock()

	if model == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Модель не обучена. Нажмите «Обучить» на вкладке ML."})
		return
	}

	var req mlScanRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	symbols := req.Symbols
	if len(symbols) == 0 {
		symbols = defaultScanSymbols
	}
	strategies := req.Strategies
	if len(strategies) == 0 {
		strategies = defaultScanStrategies
	}
	dtes := req.DTEs
	dteSource := "custom"
	if len(dtes) == 0 {
		// Default: real exchange expiries, not the fixed grid — weeklies,
		// monthlies and quarterlies all take part.
		dtes, dteSource = liveSeriesDTEs(symbols)
	}
	ivs := req.IVs
	if len(ivs) == 0 {
		ivs = defaultScanIVs
	}
	trends := req.Trends
	if len(trends) == 0 {
		trends = defaultScanTrends
	}
	vols := req.Vols
	if len(vols) == 0 {
		vols = defaultScanVols
	}
	top := req.Top
	if top <= 0 {
		top = 8
	}
	if top > 20 {
		top = 20
	}
	// Validate axes: known symbols/strategies only, sane ranges, grid cap.
	for _, s := range symbols {
		if _, ok := symbolEncodings[s]; !ok {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "неизвестный инструмент: " + s})
			return
		}
	}
	for _, s := range strategies {
		if _, ok := encodeStrategyName(s); !ok {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "неизвестная стратегия: " + s})
			return
		}
	}
	for _, d := range dtes {
		if d < 1 || d > 365 {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "DTE вне 1..365"})
			return
		}
	}
	for _, v := range ivs {
		if v < 1 || v > 300 {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "IV вне 1..300"})
			return
		}
	}
	if len(symbols)*len(strategies)*len(dtes)*len(ivs)*len(trends)*len(vols) > 20000 {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "сетка больше 20000 комбинаций — сузьте оси"})
		return
	}

	scanned := len(symbols) * len(strategies) * len(dtes) * len(ivs) * len(trends) * len(vols)
	if req.PerTenor > 0 {
		per := req.PerTenor
		if per > 10 {
			per = 10
		}
		groups := scanMLGrouped(model, symbols, strategies, dtes, ivs, trends, vols, per)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"groups": groups, "scanned": scanned,
			"train_size": model.TrainSize, "dtes": dtes, "dte_source": dteSource,
		})
		return
	}
	rows := scanMLCombinations(model, symbols, strategies, dtes, ivs, trends, vols, top)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"rows": rows, "scanned": scanned,
		"train_size": model.TrainSize, "dtes": dtes, "dte_source": dteSource,
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
		"trained":            true,
		"trained_at":         trained,
		"accuracy":           model.Accuracy,
		"precision":          model.Precision,
		"recall":             model.Recall,
		"f1":                 model.F1,
		"train_size":         model.TrainSize,
		"feature_importance": fis,
		"available_trades":   len(quant.GetTrades()),
	})
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
