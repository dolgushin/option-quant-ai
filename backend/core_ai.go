package main

// Analytics core, AI layer: OpenAI-compatible consultant over the market
// brief, verdict journal, scheduled auto-scan and optional paper auto-entry.
// The quant scoring always runs; the LLM adds a second opinion when an API
// key is configured. The final decision always stays with the user unless
// auto-paper mode is explicitly enabled.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type coreAISettings struct {
	BaseURL     string  `json:"base_url"` // e.g. https://api.openai.com/v1
	APIKey      string  `json:"api_key"`
	Model       string  `json:"model"`        // e.g. gpt-4o-mini
	IntervalMin int     `json:"interval_min"` // auto-scan interval, 0 = off
	AutoPaper   bool    `json:"auto_paper"`   // auto-open paper spreads on strong verdicts
	QuantWeight float64 `json:"quant_weight"` // квантовый вес (0.0–1.0), по умолчанию 0.7
	AiWeight    float64 `json:"ai_weight"`    // ИИ‑вес (0.0–1.0), по умолчанию 0.3
}

type coreAIVerdict struct {
	Trade          bool    `json:"trade"`
	Confidence     float64 `json:"confidence"` // 0..1
	Construction   string  `json:"construction"`
	Symbol         string  `json:"symbol"`
	Expiry         string  `json:"expiry"`
	EntryPlan      string  `json:"entry_plan"`
	ManagementPlan string  `json:"management_plan"`
	HedgePlan      string  `json:"hedge_plan"`
	Invalidation   string  `json:"invalidation"`
	Reasoning      string  `json:"reasoning"`
}

type coreVerdict struct {
	At        time.Time      `json:"at"`
	Mode      string         `json:"mode"` // quant | ai
	Brief     *coreBrief     `json:"brief"`
	QuantTop  *coreCandidate `json:"quant_top,omitempty"`
	AI        *coreAIVerdict `json:"ai,omitempty"`
	AIError   string         `json:"ai_error,omitempty"`
	PaperOpen string         `json:"paper_open,omitempty"`
}

var (
	coreMu         sync.Mutex
	coreSet        = coreAISettings{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini"}
	coreVerdictLog []coreVerdict
	coreFile       string
	coreScanOn     bool
)

func initCore(dataDir string) {
	coreFile = filepath.Join(dataDir, "core_state.json")
	if b, err := os.ReadFile(coreFile); err == nil {
		var st struct {
			Settings        coreAISettings `json:"settings"`
			Verdicts        []coreVerdict  `json:"verdicts"`
			LastNotifiedKey string         `json:"last_notified_key"`
		}
		if json.Unmarshal(b, &st) == nil {
			coreSet = st.Settings
			coreVerdictLog = st.Verdicts
			if st.LastNotifiedKey != "" {
				lastCandidateKeyMu.Lock()
				lastCandidateKey = st.LastNotifiedKey
				lastCandidateKeyMu.Unlock()
			}
		}
	}
	// Устанавливаем значения по умолчанию, если они не загружены из файла
	if coreSet.QuantWeight == 0 {
		coreSet.QuantWeight = 0.7
	}
	if coreSet.AiWeight == 0 {
		coreSet.AiWeight = 0.3
	}
	coreStartAutoScan()
}

func saveCoreStateLocked() {
	if coreFile == "" {
		return
	}
	lastCandidateKeyMu.Lock()
	notifiedKey := lastCandidateKey
	lastCandidateKeyMu.Unlock()
	b, _ := json.MarshalIndent(struct {
		Settings        coreAISettings `json:"settings"`
		Verdicts        []coreVerdict  `json:"verdicts"`
		LastNotifiedKey string         `json:"last_notified_key"`
	}{coreSet, coreVerdictLog, notifiedKey}, "", "  ")
	_ = os.WriteFile(coreFile, b, 0600)
}

const coreKBDigest = `Ты — опционный аналитик MOEX. Правила базы знаний:
Вход: 14–45 DTE; шорт-страйк 16–25Δ; кредит ≥ 1/3 ширины крыла; стаканы ≤10% mid.
Продавать премию при IV−HV ≥ +5 п.п. или IV Rank ≥ 40; покупать при IV−HV ≤ −5.
Тренд: бычьи структуры по восходящему тренду, медвежьи по нисходящему; боковик — кредитные вне денег.
Управление: T/P 50–75% макс. прибыли; стоп при 1.5–2× кредита; ролл только за нет-кредит;
TPR 1σ: рост→лестница, боковик→ratio, падение→ATM put; короткая гамма у экспирации — time-stop.
Риск: не увеличивать размер в убытке; голые хвосты запрещены; дивидендный гэп SBER (май–июль).
Ответь СТРОГО JSON-объектом по схеме:
{"trade": bool, "confidence": 0..1, "construction": "...", "symbol": "...", "expiry": "YYYY-MM-DD",
"entry_plan": "...", "management_plan": "...", "hedge_plan": "...", "invalidation": "...", "reasoning": "..."}`

// AI quiet hours (MSK). Outside 09:00–22:00 the LLM is not consulted at all.
const (
	aiQuietStartHour = 22
	aiQuietEndHour   = 9
)

// inAiQuietHours reports whether the given local hour (0–23) falls into the
// nightly window [aiQuietStartHour, 24) ∪ [0, aiQuietEndHour).
func inAiQuietHours(hour int) bool {
	return hour >= aiQuietStartHour || hour < aiQuietEndHour
}

// aiQuietHoursNow reports whether the LLM should stay offline right now,
// evaluated in Moscow time (MSK, UTC+3) regardless of the server locale.
func aiQuietHoursNow() bool {
	loc := time.FixedZone("MSK", 3*60*60)
	return inAiQuietHours(time.Now().In(loc).Hour())
}

// callLLM consults an OpenAI-compatible chat completions endpoint.
func callLLM(brief *coreBrief) (*coreAIVerdict, error) {
	if coreSet.APIKey == "" || coreSet.BaseURL == "" {
		return nil, fmt.Errorf("AI API не настроен (нужен base_url и api_key)")
	}
	if aiQuietHoursNow() {
		return nil, fmt.Errorf("ИИ выключен в ночное время (22:00–09:00 MSK)")
	}
	briefJSON, _ := json.Marshal(brief)
	payload := map[string]interface{}{
		"model": coreSet.Model,
		"messages": []map[string]string{
			{"role": "system", "content": coreKBDigest},
			{"role": "user", "content": string(briefJSON)},
		},
		"temperature":     0.2,
		"response_format": map[string]string{"type": "json_object"},
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(coreSet.BaseURL, "/") + "/chat/completions"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+coreSet.APIKey)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("AI API: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("AI API: пустой ответ")
	}
	return parseAIVerdict(out.Choices[0].Message.Content)
}

// parseAIVerdict tolerantly extracts the verdict JSON from a model reply.
func parseAIVerdict(content string) (*coreAIVerdict, error) {
	s := strings.TrimSpace(content)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	var v coreAIVerdict
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("не удалось разобрать ответ ИИ: %w", err)
	}
	v.Reasoning = strings.TrimSpace(v.Reasoning)
	return &v, nil
}

// runCoreAnalysis builds the brief, asks the AI (when configured) and stores
// the verdict; optionally opens a paper spread on strong quant+AI agreement.
func runCoreAnalysis(force bool) (*coreVerdict, error) {
	brief := collectCoreBrief(force)
	v := coreVerdict{At: time.Now(), Mode: "quant", Brief: brief}
	if len(brief.Candidates) > 0 {
		top := brief.Candidates[0]
		v.QuantTop = &top
	}

	ai, err := callLLM(brief)
	if err != nil {
		v.AIError = err.Error()
	} else {
		v.AI = ai
		v.Mode = "ai"
	}

	coreMu.Lock()
	coreVerdictLog = append([]coreVerdict{v}, coreVerdictLog...)
	if len(coreVerdictLog) > 50 {
		coreVerdictLog = coreVerdictLog[:50]
	}
	saveCoreStateLocked()
	settings := coreSet
	coreMu.Unlock()

	if settings.AutoPaper && shouldAutoOpen(&v) {
		if id := openPaperFromVerdict(v); id != "" {
			v.PaperOpen = id
		}
	}
	return &v, nil
}

// shouldAutoOpen requires quant and AI agreement plus a high score.
func shouldAutoOpen(v *coreVerdict) bool {
	if v.QuantTop == nil || v.QuantTop.Score < 70 || v.AI == nil || !v.AI.Trade {
		return false
	}
	if v.AI.Construction != "" && v.QuantTop.Strategy != "" {
		a := strings.ToLower(v.AI.Construction)
		b := strings.ToLower(v.QuantTop.Strategy)
		if !(strings.Contains(a, b) || strings.Contains(b, a)) {
			return false
		}
	}
	return v.AI.Confidence >= 0.6
}

// openPaperFromVerdict opens the top quant candidate as a paper spread and
// pushes a Telegram alert with the entry details. Each option leg is priced at
// its executable book level (SELL fills the best bid, BUY the best ask) so the
// recorded entry reflects what the market actually pays for an immediate order;
// exit/close continues to use theoretical marks (see priceOpenLegsAtMOEXTheo).
func openPaperFromVerdict(v coreVerdict) string {
	top := v.QuantTop
	plan, err := buildVerticalSpread(top.Symbol, top.Strategy, top.Expiry, 1)
	if err != nil {
		return ""
	}
	priceEntryAtExecutable(plan)
	src := spreadRecord{
		StopLossPct: 0.75, AutoRollDTE: defaultAutoRollDTE(plan.DaysToExp),
		RollCreditPct: 0.5, RollStrikeRiskPct: 0.03,
		ProfitTargetPct: 0.75, ProfitAction: "CLOSE",
		TPRMode: "ONE_DAY_SIGMA", TPRSigmaMult: 1, SigmaAnnual: 0.30, RollAlpha: 1,
	}
	rec, err := createFromPlan(plan, &src, 1, 0)
	if err != nil {
		return ""
	}
	if !spreadManagerEnabled() {
		startSpreadManager()
	}
	notifyStructureEntry(rec, plan)
	return rec.ID
}

// priceEntryAtExecutable re-marks a plan's option legs at the executable
// prices from the live Alor order book before opening: a SELL leg fills at the
// best bid, a BUY leg at the best ask (both are what an immediate market order
// would actually pay/receive). Legs without a usable book level keep the plan
// mid price. Plan economics (credit/debit, extremes, margin) are recomputed so
// the recorded spread matches the market entry, while exit stays theoretical.
func priceEntryAtExecutable(plan *spreadPlan) {
	if plan == nil || alorMarket == nil {
		return
	}
	changed := false
	for i := range plan.Legs {
		l := &plan.Legs[i]
		ob, err := alorMarket.FetchOrderbook("MOEX", l.SecID)
		if err != nil {
			continue
		}
		var fill float64
		if l.Side == "SELL" && len(ob.Bids) > 0 && ob.Bids[0].Price > 0 {
			fill = ob.Bids[0].Price
		} else if l.Side == "BUY" && len(ob.Asks) > 0 && ob.Asks[0].Price > 0 {
			fill = ob.Asks[0].Price
		}
		if fill <= 0 {
			continue
		}
		l.Price = math.Round(fill*100) / 100
		changed = true
	}
	if changed {
		recomputePlanEconomics(plan)
	}
}

// recomputePlanEconomics re-derives a plan's credit/debit, max profit/loss and
// margin from the leg prices, mirroring buildVerticalSpread.
func recomputePlanEconomics(plan *spreadPlan) {
	if plan == nil {
		return
	}
	var credit, debit, marginShort float64
	for _, l := range plan.Legs {
		if l.Side == "SELL" {
			credit += l.Price
			marginShort += l.MarginShort
		} else {
			debit += l.Price
		}
	}
	wing := math.Abs(plan.ShortStrike - plan.LongStrike)
	if wing <= 0 {
		wing = 1
	}
	netCredit := credit - debit
	var maxProfit, maxLoss float64
	if plan.IsDebit {
		netDebit := debit - credit
		if netDebit < 0 {
			netDebit = 0
		}
		maxProfit = wing - netDebit
		maxLoss = netDebit
	} else {
		maxProfit = netCredit
		maxLoss = wing - netCredit
	}
	if maxProfit < 0 {
		maxProfit = 0
	}
	if maxLoss < 0 {
		maxLoss = 0
	}
	q := float64(plan.Qty)
	plan.NetCredit = math.Round(netCredit*q*100) / 100
	plan.MaxProfit = math.Round(maxProfit*q*100) / 100
	plan.MaxLoss = math.Round(maxLoss*q*100) / 100
	plan.MarginShort = math.Round(marginShort*q*100) / 100
}

// notifyStructureEntry pushes a Telegram alert when the Core auto-entry opened
// a new paper structure (автовход с согласием кванта и ИИ). Sends a payoff
// chart photo plus a full-parameter caption.
func notifyStructureEntry(rec *spreadRecord, plan *spreadPlan) {
	telegramMu.RLock()
	configured := telegramToken != "" && telegramChatID != ""
	telegramMu.RUnlock()
	if !configured || rec == nil || plan == nil {
		return
	}

	pts := candidatePayoff(plan)
	name := rec.DisplayName
	if name == "" {
		name = plan.DisplayName
	}
	img, err := drawPayoffChart(chartTitle(plan.Symbol, name, plan.Expiry), pts, plan.Spot, plan.ShortStrike, plan.LongStrike)
	if err != nil || len(img) == 0 {
		logTelegramErr("auto-entry-text", sendTelegramMessage(entryCaption(rec, plan)))
		return
	}
	logTelegramErr("auto-entry-photo", sendTelegramPhoto(entryCaption(rec, plan), img))
}

// formatRub renders a ruble amount with space grouping: 86184 -> "86 184",
// 1220.5 with 2 decimals -> "1 220.50".
func formatRub(v float64, dec int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatFloat(v, 'f', dec, 64)
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	n := len(intPart)
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteByte(intPart[i])
	}
	out := b.String()
	if dec > 0 {
		out += "." + fracPart
	}
	if neg {
		out = "-" + out
	}
	return out
}

// captionBullets splits reason strings (which may themselves contain " · ")
// into one bullet per line.
func captionBullets(reasons []string) string {
	var lines []string
	for _, r := range reasons {
		for _, part := range strings.Split(r, " · ") {
			if p := strings.TrimSpace(part); p != "" {
				lines = append(lines, "• "+telegramEscape(p))
			}
		}
	}
	return strings.Join(lines, "\n")
}

// entryCaption formats the auto-entry Telegram caption from the opened paper
// spread record and its plan (economics already marked at executable prices).
func entryCaption(rec *spreadRecord, plan *spreadPlan) string {
	credit := plan.NetCredit
	kind := "Кредит"
	if credit < 0 {
		kind = "Дебет"
		credit = -credit
	}
	name := rec.DisplayName
	if name == "" {
		name = plan.DisplayName
	}
	var sb strings.Builder
	sb.WriteString("🟢 <b>Автовход Ядро</b>\n")
	sb.WriteString("<b>" + telegramEscape(name) + " · " + telegramEscape(plan.Symbol) + "</b>\n")
	sb.WriteString(fmt.Sprintf("📅 Экспирация: %s (DTE %d)\n", telegramEscape(plan.Expiry), plan.DaysToExp))
	sb.WriteString(fmt.Sprintf("📍 Вход-спот: %s\n", formatRub(plan.Spot, 0)))
	sb.WriteString(fmt.Sprintf("📉 Шорт: %s / Лонг: %s\n", formatRub(plan.ShortStrike, 0), formatRub(plan.LongStrike, 0)))
	sb.WriteString(fmt.Sprintf("💰 %s: %s ₽\n", kind, formatRub(credit, 2)))
	sb.WriteString(fmt.Sprintf("📈 Макс. прибыль: %s ₽\n", formatRub(plan.MaxProfit, 0)))
	sb.WriteString(fmt.Sprintf("📉 Макс. убыток: %s ₽\n", formatRub(plan.MaxLoss, 0)))
	sb.WriteString(fmt.Sprintf("⚙️ Управление: авторолл при DTE≤%d · стоп %0.0f%% · цель %0.0f%%", rec.AutoRollDTE, rec.StopLossPct*100, rec.ProfitTargetPct*100))
	return sb.String()
}

// lastCandidateKey is the dedup key of the last candidate chart pushed to
// Telegram, so the periodic auto-scan does not resend the same construction.
var (
	lastCandidateKeyMu sync.Mutex
	lastCandidateKey   string
)

// candidateDedupKey identifies a found construction regardless of score changes:
// symbol + strategy + expiry + strike pair. Same construction found again on the
// next scan is suppressed.
func candidateDedupKey(c *coreCandidate) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s|%s|%s|%.0f|%.0f", c.Symbol, c.Strategy, c.Expiry, c.ShortStrike, c.LongStrike)
}

// markCandidateNotified records a successfully pushed construction (in-memory
// + persisted) so neither the next scan nor a restart resends it.
func markCandidateNotified(key string) {
	if key == "" {
		return
	}
	lastCandidateKeyMu.Lock()
	lastCandidateKey = key
	lastCandidateKeyMu.Unlock()
	coreMu.Lock()
	saveCoreStateLocked()
	coreMu.Unlock()
}

// notifyCandidateSpread pushes a Telegram photo of the spread payoff plus a
// caption with every parameter of the found candidate. Deduped: a construction
// already pushed is not resent (the scan may find the same one on every pass).
// The dedup key is recorded only after a successful send, so a failed push is
// retried on the next scan instead of being silently lost. Reports whether a
// fresh alert actually went out (false = suppressed duplicate, failure, or no
// Telegram config).
func notifyCandidateSpread(c *coreCandidate) bool {
	if c == nil {
		return false
	}
	telegramMu.RLock()
	configured := telegramToken != "" && telegramChatID != ""
	telegramMu.RUnlock()
	if !configured {
		return false
	}

	key := candidateDedupKey(c)
	if key != "" {
		lastCandidateKeyMu.Lock()
		dup := key == lastCandidateKey
		lastCandidateKeyMu.Unlock()
		if dup {
			return false
		}
	}

	plan, err := buildVerticalSpread(c.Symbol, c.Strategy, c.Expiry, 1)
	if err != nil {
		// Falls back to a text-only alert if the plan cannot be rebuilt.
		return sendCandidateText(c, key)
	}

	pts := candidatePayoff(plan)
	img, err := drawPayoffChart(chartTitle(c.Symbol, c.DisplayName, c.Expiry), pts, plan.Spot, plan.ShortStrike, plan.LongStrike)
	if err != nil || len(img) == 0 {
		return sendCandidateText(c, key)
	}

	caption := candidateCaption(c, plan)
	if err := sendTelegramPhoto(caption, img); err != nil {
		logTelegramErr("candidate-photo", err)
		return false
	}
	markCandidateNotified(key)
	return true
}

// candidateCaption formats a rich HTML caption with every candidate parameter.
func candidateCaption(c *coreCandidate, plan *spreadPlan) string {
	credit := c.NetCredit
	kind := "Кредит"
	if credit < 0 {
		kind = "Дебет"
		credit = -credit
	}
	var sb strings.Builder
	sb.WriteString("🎯 <b>Найдено Ядром</b>\n")
	sb.WriteString("<b>" + telegramEscape(c.DisplayName) + " · " + telegramEscape(c.Symbol) + "</b>\n")
	sb.WriteString(fmt.Sprintf("📅 Экспирация: %s (DTE %d)\n", telegramEscape(c.Expiry), c.DTE))
	sb.WriteString(fmt.Sprintf("📍 Спот: %s\n", formatRub(plan.Spot, 0)))
	sb.WriteString(fmt.Sprintf("📉 Шорт: %s / Лонг: %s\n", formatRub(c.ShortStrike, 0), formatRub(c.LongStrike, 0)))
	sb.WriteString(fmt.Sprintf("💰 %s: %s ₽\n", kind, formatRub(credit, 2)))
	sb.WriteString(fmt.Sprintf("📈 Макс. прибыль: %s ₽\n", formatRub(c.MaxProfit, 0)))
	sb.WriteString(fmt.Sprintf("📉 Макс. убыток: %s ₽\n", formatRub(c.MaxLoss, 0)))
	sb.WriteString(fmt.Sprintf("🎲 Шанс прибыли: %d%%\n", c.PopProb))
	sb.WriteString(fmt.Sprintf("⭐ Оценка: %d/100", c.Score))
	if bullets := captionBullets(c.Reasons); bullets != "" {
		sb.WriteString("\n📝 Причины:\n" + bullets)
	}
	return sb.String()
}

// sendCandidateText is a fallback when the chart cannot be rendered.
func sendCandidateText(c *coreCandidate, key string) bool {
	plan, err := buildVerticalSpread(c.Symbol, c.Strategy, c.Expiry, 1)
	if err != nil {
		return false
	}
	if err := sendTelegramMessage(candidateCaption(c, plan)); err != nil {
		logTelegramErr("candidate-text", err)
		return false
	}
	markCandidateNotified(key)
	return true
}

// scanVerdictNeedsText decides whether the generic scan line adds anything
// over the candidate chart: skip it when the chart was just pushed and there
// is no AI trade signal to report. Paper opens already have their dedicated
// entry message, so they never need the generic line either.
func scanVerdictNeedsText(pushed bool, v *coreVerdict) bool {
	if v == nil || v.PaperOpen != "" {
		return false
	}
	if pushed && (v.AI == nil || !v.AI.Trade) {
		return false
	}
	return true
}

// countPricedInstruments counts brief instruments with a live spot — zero
// means the market-data fetch failed and the scan ran blind. Pure.
func countPricedInstruments(b *coreBrief) int {
	if b == nil {
		return 0
	}
	n := 0
	for _, in := range b.Instruments {
		if in.Spot > 0 {
			n++
		}
	}
	return n
}
func coreAutoScanLoop() {
	for {
		coreMu.Lock()
		enabled := coreSet.IntervalMin > 0
		interval := time.Duration(coreSet.IntervalMin) * time.Minute
		coreMu.Unlock()
		if !enabled {
			time.Sleep(30 * time.Second)
			continue
		}
		if v, err := runCoreAnalysis(false); err != nil {
			log.Printf("core: auto-scan failed: %v", err)
		} else {
			// runCoreAnalysis never returns an error today — the real
			// outage signal is a brief with no priced instruments (MOEX
			// unreachable): candidates are empty and Telegram stays silent.
			if countPricedInstruments(v.Brief) == 0 {
				log.Printf("core: auto-scan has no market data (MOEX unreachable?)")
			}
			// Push a chart of the top found construction (deduped) so the
			// trader sees what the scan actually found, not just a score line.
			pushed := false
			if v.QuantTop != nil {
				pushed = notifyCandidateSpread(v.QuantTop)
			}
			txt := fmt.Sprintf("🧠 Ядро: вердикт %s", v.Mode)
			if v.QuantTop != nil {
				txt += fmt.Sprintf(" | топ: %s %s (%d)", v.QuantTop.Symbol, v.QuantTop.DisplayName, v.QuantTop.Score)
			}
			if v.AI != nil && v.AI.Trade {
				txt += " | ИИ: ВХОД"
			}
			// Skip the generic line when it would only repeat the chart that
			// just went out (paper entries have their own 🟢 message anyway).
			if scanVerdictNeedsText(pushed, v) {
				logTelegramErr("scan-verdict", sendTelegramMessage(txt))
			}
		}
		time.Sleep(interval)
	}
}

func coreStartAutoScan() {
	coreMu.Lock()
	if coreScanOn {
		coreMu.Unlock()
		return
	}
	coreScanOn = true
	coreMu.Unlock()
	go coreAutoScanLoop()
}

// ---- handlers ----

// GET/POST /api/v2/core/settings
func coreSettingsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	coreMu.Lock()
	defer coreMu.Unlock()
	if r.Method == http.MethodPost {
		var req coreAISettings
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			return
		}
		if req.BaseURL != "" {
			coreSet.BaseURL = strings.TrimRight(req.BaseURL, "/")
		}
		if req.APIKey != "" {
			coreSet.APIKey = req.APIKey
		}
		if req.Model != "" {
			coreSet.Model = req.Model
		}
		coreSet.IntervalMin = int(math.Max(0, math.Min(float64(req.IntervalMin), 1440)))
		coreSet.AutoPaper = req.AutoPaper
		coreSet.QuantWeight = req.QuantWeight
		coreSet.AiWeight = req.AiWeight
		saveCoreStateLocked()
	}
	out := coreSet
	if out.APIKey != "" {
		out.APIKey = "••••" + out.APIKey[maxInt(0, len(out.APIKey)-4):]
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"settings": out})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// POST /api/v2/core/analyze
func coreAnalyzeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	v, err := runCoreAnalysis(true)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "verdict": v})
}

// GET /api/v2/core/verdicts
func coreVerdictsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	coreMu.Lock()
	defer coreMu.Unlock()
	if coreVerdictLog == nil {
		coreVerdictLog = []coreVerdict{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"verdicts": coreVerdictLog})
}
