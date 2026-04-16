package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xray-panel/internal/middleware"
	"xray-panel/internal/paysign"
	"xray-panel/internal/tariffs"
)

type PayHandler struct {
	renderer      *Renderer
	payServiceURL string // https://xstreamka.dev
	panelBaseURL  string // https://panel.xstreamka.dev — куда Робокасса/pay-service вернут пользователя и webhook
	webhookSecret string
}

func NewPayHandler(renderer *Renderer, payServiceURL, panelBaseURL, webhookSecret string) *PayHandler {
	return &PayHandler{
		renderer:      renderer,
		payServiceURL: strings.TrimRight(payServiceURL, "/"),
		panelBaseURL:  strings.TrimRight(panelBaseURL, "/"),
		webhookSecret: webhookSecret,
	}
}

// Index — GET /pay — страница тарифов + сообщение о результате оплаты (?payment=success|failed&inv_id=…)
func (h *PayHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	// Блокируем страницу если конфиг не настроен — так видно на dev-стенде.
	if h.payServiceURL == "" || h.webhookSecret == "" {
		http.Error(w, "Оплата временно недоступна: PAY_SERVICE_URL или WEBHOOK_SECRET не настроены", http.StatusServiceUnavailable)
		return
	}

	h.renderer.Render(w, "pay.html", map[string]any{
		"User":    user,
		"Tariffs": tariffs.List,
		"Status":  r.URL.Query().Get("payment"), // success | failed | ""
		"InvID":   r.URL.Query().Get("inv_id"),
	})
}

// Checkout — POST /pay/checkout — формирует подписанную ссылку и редиректит на pay-service.
func (h *PayHandler) Checkout(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	if h.payServiceURL == "" || h.webhookSecret == "" {
		http.Error(w, "Оплата не настроена", http.StatusServiceUnavailable)
		return
	}

	planID := r.FormValue("plan_id")
	tariff := tariffs.FindByID(planID)
	if tariff == nil {
		http.Error(w, "Неизвестный тариф", http.StatusBadRequest)
		return
	}

	// metadata вернётся к нам в webhook → по traffic_gb начислим баланс
	metadata, _ := json.Marshal(map[string]any{
		"traffic_gb": tariff.TrafficGB,
	})

	params := map[string]string{
		"product_type": "vpn",
		"plan_id":      tariff.ID,
		"amount":       fmt.Sprintf("%.2f", tariff.Amount),
		"description":  tariff.Description,
		"user_ref":     strconv.Itoa(user.ID),
		"email":        user.Email,
		"callback_url": h.panelBaseURL + "/api/payments/webhook",
		"return_url":   h.panelBaseURL + "/pay",
		"metadata":     string(metadata),
		"ts":           strconv.FormatInt(time.Now().Unix(), 10),
	}

	sig := paysign.Sign(params, h.webhookSecret)

	q := make(url.Values, len(params)+1)
	for k, v := range params {
		q.Set(k, v)
	}
	q.Set("sig", sig)

	checkoutURL := h.payServiceURL + "/pay/checkout?" + q.Encode()

	log.Printf("Pay: user %d (%s) → checkout %s (%.2f ₽)", user.ID, user.Email, tariff.ID, tariff.Amount)

	http.Redirect(w, r, checkoutURL, http.StatusSeeOther)
}
