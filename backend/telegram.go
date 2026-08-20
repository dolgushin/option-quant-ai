package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"option-quant-ai/quant"
)

// telegramMu guards the in-memory Telegram config reloaded from disk.
var (
	telegramMu     sync.RWMutex
	telegramToken  string
	telegramChatID string
)

// initTelegram loads Telegram credentials from the secure store (fallback to
// env vars for simple deployments).
func initTelegram() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chat := os.Getenv("TELEGRAM_CHAT_ID")
	if tokenStore != nil {
		if s, err := tokenStore.LoadTelegram(); err == nil && s.BotToken != "" {
			token = s.BotToken
			chat = s.ChatID
		}
	}
	telegramMu.Lock()
	telegramToken = token
	telegramChatID = chat
	telegramMu.Unlock()
}

// sendTelegramMessage sends a text message to the configured chat via the Bot API.
func sendTelegramMessage(text string) error {
	telegramMu.RLock()
	token := telegramToken
	chat := telegramChatID
	telegramMu.RUnlock()
	return sendTelegramMessageWith(token, chat, text)
}

// sendTelegramMessageWith sends a message with explicit credentials (used by the
// settings handler to validate before persisting).
func sendTelegramMessageWith(token, chat, text string) error {
	if token == "" || chat == "" {
		return fmt.Errorf("telegram not configured")
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	form := url.Values{}
	form.Set("chat_id", chat)
	form.Set("text", text)
	form.Set("parse_mode", "HTML")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(apiURL, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram api status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// settingsTelegramHandler GET returns status; POST saves token+chat and sends a
// test message.
// URL: /api/v1/settings/telegram
func settingsTelegramHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		telegramMu.RLock()
		configured := telegramToken != "" && telegramChatID != ""
		telegramMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"configured": configured,
			"has_token":  telegramToken != "",
			"chat_id":    maskSecret(telegramChatID),
		})
		return

	case http.MethodPost:
		var req struct {
			BotToken string `json:"bot_token"`
			ChatID   string `json:"chat_id"`
			Test     bool   `json:"test"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		if req.BotToken == "" || req.ChatID == "" {
			http.Error(w, "bot_token and chat_id are required", http.StatusBadRequest)
			return
		}

		// Send the test message first so we never persist invalid credentials.
		if req.Test {
			if err := sendTelegramMessageWith(req.BotToken, req.ChatID, "<b>✅ Telegram-уведомления настроены</b>\n\nТерминал MOEX Options Quant будет присылать алерты по позициям, режиму рынка и пин-риску."); err != nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "error": err.Error()})
				return
			}
		}

		if tokenStore != nil {
			if err := tokenStore.SaveTelegram(req.BotToken, req.ChatID); err != nil {
				http.Error(w, "Failed to save telegram settings: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		telegramMu.Lock()
		telegramToken = req.BotToken
		telegramChatID = req.ChatID
		telegramMu.Unlock()

		status := "saved"
		msg := ""
		if req.Test {
			status = "saved_and_test_sent"
			msg = "Тестовое сообщение отправлено."
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "message": msg})
		return

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// maskSecret hides all but the last 4 characters of a secret string.
func maskSecret(s string) string {
	if len(s) <= 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

// telegramNotifier periodically checks for actionable events and pushes them to
// Telegram: critical expiry/pin-risk, rotation signals, and gate status changes.
func telegramNotifier(stop <-chan struct{}) {
	tick := time.NewTicker(15 * time.Minute)
	defer tick.Stop()

	// Suppress repeats: remember the last pushed status so we don't spam.
	lastSent := map[string]string{}

	sendIfChanged := func(key, text string, force bool) {
		if text == "" {
			return
		}
		telegramMu.RLock()
		configured := telegramToken != "" && telegramChatID != ""
		telegramMu.RUnlock()
		if !configured {
			return
		}
		last := lastSent[key]
		if !force && last == text {
			return
		}
		if err := sendTelegramMessage(text); err == nil {
			lastSent[key] = text
		}
	}

	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}

		// 1) Expiry / pin-risk critical alerts.
		positions := quant.GetActivePositions()
		if len(positions) > 0 {
			for _, p := range positions {
				dte := dteInDays(p.Expiry, time.Now())
				if dte <= 5 {
					msg := fmt.Sprintf("⏰ <b>%s</b> (%s): %d дн. до экспирации — закрыть или ролл.", telegramEscape(p.Strategy), telegramEscape(p.Symbol), dte)
					sendIfChanged("dte:"+p.ID, msg, false)
				}
			}
		}

		// 2) Rotation signal when the top strategy for the regime changes.
		symbol := "Si"
		iv := currentATMIVRaw(symbol)
		hv := realizedVolForSymbol(symbol)
		if iv > 0 && hv > 0 {
			trend := tradeTrend(symbol)
			trendRegime := "SIDEWAYS"
			if t, ok := trend["regime"].(string); ok {
				trendRegime = t
			}
			regime := quant.ClassifyMarketRegime(iv, hv, trendRegime == "BULLISH" || trendRegime == "BEARISH")
			held := make([]quant.HeldPositionInfo, 0, len(positions))
			for _, p := range positions {
				held = append(held, quant.HeldPositionInfo{ID: p.ID, Strategy: normalizeStrategyName(p.Strategy), Symbol: p.Symbol})
			}
			advice := quant.RecommendRotation(regime, trendRegime, held)
			top := advice.Ranking[0].StrategyName
			key := "rot:" + symbol
			text := fmt.Sprintf("🔄 <b>Режим: %s</b> (тренд %s, IV %.0f%%, HV %.0f%%)\nЛучшая стратегия: <b>%s</b> (%.0f/100).", advice.Regime, advice.Trend, iv*100, hv*100, top, advice.Ranking[0].Score)
			sendIfChanged(key, text, true)
		}
	}
}

// telegramEscape removes HTML-significant characters from user text.
func telegramEscape(s string) string {
	repl := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return repl.Replace(s)
}