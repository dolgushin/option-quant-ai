package agent

import (
	"fmt"
	"strings"
)

type CopilotRequest struct {
	Prompt string  `json:"prompt"`
	Delta  float64 `json:"delta"`
	Theta  float64 `json:"theta"`
	Gamma  float64 `json:"gamma"`
	Vega   float64 `json:"vega"`
	Spread float64 `json:"spread"`
}

type CopilotResponse struct {
	Reply        string                 `json:"reply"`
	Action       string                 `json:"action"`
	ToolData     map[string]interface{5} `json:"tool_data"`
}

func ProcessCopilotQuery(req CopilotRequest) CopilotResponse {
	promptLower := strings.ToLower(req.Prompt)

	toolData := map[string]interface{}{
		"delta": req.Delta,
		"theta": req.Theta,
		"gamma": req.Gamma,
		"vega":  req.Vega,
	}

	// Keyword / Intent routing for Agent Skills
	if strings.Contains(promptLower, "дельта") || strings.Contains(promptLower, "хедж") || strings.Contains(promptLower, "нейтраль") {
		hedgeContracts := int(req.Delta * -10)
		action := "HOLD"
		advice := fmt.Sprintf("Твоя текущая Дельта составляет %.2f. ", req.Delta)
		if req.Delta > 0.05 {
			action = fmt.Sprintf("SELL %d FUTURES", int(req.Delta*10))
			advice += fmt.Sprintf("Портфель имеет направленный лонг. Чтобы вернуть дельта-нейтральность (Δ=0.00), рекомендуется продать ~%d контрактов базового актива (Si/RI/CR).", int(req.Delta*10))
		} else if req.Delta < -0.05 {
			action = fmt.Sprintf("BUY %d FUTURES", int(-req.Delta*10))
			advice += fmt.Sprintf("Портфель имеет направленный шорт. Чтобы вернуть дельта-нейтральность (Δ=0.00), рекомендуется купить ~%d контрактов базового актива.", int(-req.Delta*10))
		} else {
			advice += "Портфель находится в идеальной дельта-нейтральной зоне. Дополнительное хеджирование не требуется."
		}

		toolData["recommended_action"] = action
		return CopilotResponse{
			Reply:    advice,
			Action:   action,
			ToolData: toolData,
		}
	}

	if strings.Contains(promptLower, "тета") || strings.Contains(promptLower, "распад") || strings.Contains(promptLower, "доход") {
		dailyIncome := req.Theta * 365 / 12 // approximate monthly/daily
		advice := fmt.Sprintf("Временной распад (Тета) твоего портфеля составляет Θ = %.2f в день. За счет проданных опционов ты ежедневно зарабатываешь примерно ~%.2f руб. в день, пока цена базового актива удерживается в текущем торговом коридоре.", req.Theta, dailyIncome)
		return CopilotResponse{
			Reply:    advice,
			Action:   "COLLECT_THETA",
			ToolData: toolData,
		}
	}

	if strings.Contains(promptLower, "арбитраж") || strings.Contains(promptLower, "паритет") || strings.Contains(promptLower, "спрэд") {
		advice := fmt.Sprintf("Сканирование Put-Call паритета и календарных спрэдов: текущий арбитражный спреды составляет %.2 руб. ", req.Spread)
		action := "NO_ACTION"
		if req.Spread > 10.0 {
			advice += " Обнаружена возможность конверсии (Conversion): Call-опционы переоценены относительно Put и спота. Рекомендуется продать Call, купить Put и купить спот/фьючерс."
			action = "CONVERSION_ARBITRAGE"
		} else if req.Spread < -10.0 {
			advice += " Обнаружена возможность реверса (Reversal): Put-опционы переоценены. Рекомендуется купить Call, продать Put и шортить базовый актив."
			action = "REVERSAL_ARBITRAGE"
		} else {
			advice += " Значимых арбитражных аномалий нет. Рынок сбалансирован."
		}
		return CopilotResponse{
			Reply:    advice,
			Action:   action,
			ToolData: toolData,
		}
	}

	// General portfolio & risk summary
	generalSummary := fmt.Sprintf("Квант-агент на связи. Анализ портфеля: Дельта Δ=%.2f, Тета Θ=%.2f, Гамма Γ=%.4f, Вега V=%.2f. %s",
		req.Delta, req.Theta, req.Gamma, req.Vega,
		"Портфель сбалансирован. Задавай вопросы по хеджированию, расчету греков, выбору страйков под Ratio Spreads или поиску арбитража на Si / RI / CR.")

	return CopilotResponse{
		Reply:    generalSummary,
		Action:   "ANALYZE",
		ToolData: toolData,
	}
}
