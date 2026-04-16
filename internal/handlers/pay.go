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
	"xray-panel/internal/models"
	"xray-panel/internal/paysign"
)

type PayHandler struct {
	renderer      *Renderer
	tariffs       *models.TariffStore
	payServiceURL string
	panelBaseURL  string
	webhookSecret string
}

func NewPayHandler(renderer *Renderer, tariffs *models.TariffStore, payServiceURL, panelBaseURL, webhookSecret string) *PayHandler {
	return &PayHandler{
		renderer:      renderer,
		tariffs:       tariffs,
		payServiceURL: strings.TrimRight(payServiceURL, "/"),
		panelBaseURL:  strings.TrimRight(panelBaseURL, "/"),
		webhookSecret: webhookSecret,
	}
}

func (h *PayHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	if h.payServiceURL == "" || h.webhookSecret == "" {
		http.Error(w, "Оплата временно недоступна: PAY_SERVICE_URL или WEBHOOK_SECRET не настроены", http.StatusServiceUnavailable)
		return
	}

	tariffs, err := h.tariffs.ListActive(r.Context())
	if err != nil {
		log.Printf("Pay: list tariffs error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	h.renderer.Render(w, "pay.html", map[string]any{
		"User":    user,
		"Tariffs": tariffs,
		"Status":  r.URL.Query().Get("payment"),
		"InvID":   r.URL.Query().Get("inv_id"),
	})
}

func (h *PayHandler) Checkout(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	if h.payServiceURL == "" || h.webhookSecret == "" {
		http.Error(w, "Оплата не настроена", http.StatusServiceUnavailable)
		return
	}

	code := r.FormValue("plan_id")
	tariff, err := h.tariffs.GetByCode(r.Context(), code)
	if err != nil {
		http.Error(w, "Неизвестный тариф", http.StatusBadRequest)
		return
	}
	if !tariff.IsActive {
		http.Error(w, "Тариф отключён", http.StatusBadRequest)
		return
	}

	metadata, _ := json.Marshal(map[string]any{
		"traffic_gb": tariff.TrafficGB,
	})

	params := map[string]string{
		"product_type": "vpn",
		"plan_id":      tariff.Code,
		"amount":       fmt.Sprintf("%.2f", tariff.AmountRub),
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
	log.Printf("Pay: user %d (%s) → checkout %s (%.2f ₽)",
		user.ID, user.Email, tariff.Code, tariff.AmountRub)

	http.Redirect(w, r, checkoutURL, http.StatusSeeOther)
}
