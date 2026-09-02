package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// telegramAPIBase is a Russian-hosted Telegram Bot API relay. api.telegram.org
// is unreachable from RU VPSes (connection timeout), so notifications go
// through the short proxy URL instead. The bot is bound to the relay path.
const telegramAPIBase = "http://193.233.87.23/bot8627553310"

// sendTelegramMessageWith sends a message with explicit credentials (used by the
// settings handler to validate before persisting).
func sendTelegramMessageWith(token, chat, text string) error {
	if token == "" || chat == "" {
		return fmt.Errorf("telegram not configured")
	}
	apiURL := telegramAPIBase + "/sendMessage"
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

// sendTelegramPhoto sends an in-memory PNG/JPEG photo with an HTML caption via
// the Bot API relay. Used for spread candidate charts.
func sendTelegramPhoto(caption string, pngData []byte) error {
	telegramMu.RLock()
	token := telegramToken
	chat := telegramChatID
	telegramMu.RUnlock()
	if token == "" || chat == "" {
		return fmt.Errorf("telegram not configured")
	}

	// Build the multipart/form-data body manually (mime/multipart would be
	// cleaner, but hand-building keeps behaviour explicit and version-free).
	var body bytes.Buffer
	const boundary = "oqboundary7MA4YWxkTrZu0gW"
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Disposition: form-data; name=\"chat_id\"\r\n\r\n")
	body.WriteString(chat + "\r\n")
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Disposition: form-data; name=\"parse_mode\"\r\n\r\nHTML\r\n")
	if caption != "" {
		body.WriteString("--" + boundary + "\r\n")
		body.WriteString("Content-Disposition: form-data; name=\"caption\"\r\n\r\n")
		body.WriteString(caption + "\r\n")
	}
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Disposition: form-data; name=\"photo\"; filename=\"spread.png\"\r\n")
	body.WriteString("Content-Type: image/png\r\n\r\n")
	body.Write(pngData)
	body.WriteString("\r\n")
	body.WriteString("--" + boundary + "--\r\n")

	apiURL := telegramAPIBase + "/sendPhoto"
	req, err := http.NewRequest(http.MethodPost, apiURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram api status %d: %s", resp.StatusCode, string(respBody))
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

		// Rotation/regime digests are intentionally NOT pushed to Telegram —
		// only actionable events are: expiry alerts, Core auto-entry and
		// structure closes (see notifyStructureClosed / coreAutoScanLoop).
	}
}

// telegramEscape removes HTML-significant characters from user text.
func telegramEscape(s string) string {
	repl := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return repl.Replace(s)
}

// pnlSigned formats a ruble P&L with an explicit sign.
func pnlSigned(v float64) string {
	sign := "-"
	if v >= 0 {
		sign = "+"
	}
	return fmt.Sprintf("%s%0.2f", sign, math.Abs(v))
}

// notifyStructureClosed pushes a Telegram alert when a spread structure is
// closed (manual close, stop-loss or take-profit from the auto-manager), with
// the exact data it closed at: realized P&L, return %, the close reason and
// how many days were held.
func notifyStructureClosed(s *spreadRecord, reason string, realized, pnlPct float64) {
	telegramMu.RLock()
	configured := telegramToken != "" && telegramChatID != ""
	telegramMu.RUnlock()
	if !configured || s == nil {
		return
	}
	name := s.DisplayName
	if name == "" {
		name = s.Type
	}
	if name == "" {
		name = "Структура"
	}
	days := 1
	if t, err := time.Parse("2006-01-02T15:04:05", s.OpenedAt); err == nil {
		days = int(time.Since(t).Hours() / 24)
		if days < 1 {
			days = 1
		}
	}
	txt := fmt.Sprintf("📉 <b>Структура закрыта</b>: %s · %s · эксп. %s\n%s\nРеализованный P&L: <b>%s ₽</b> (%s%.1f%%) за %d дн.",
		telegramEscape(name), telegramEscape(s.Symbol), telegramEscape(s.Expiry),
		reason,
		pnlSigned(realized), pnlSign(pnlPct), math.Abs(pnlPct), days)
	_ = sendTelegramMessage(txt)
}

func pnlSign(v float64) string {
	if v >= 0 {
		return "+"
	}
	return ""
}
