package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"xray-panel/internal/email"

	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
	"xray-panel/internal/paysign"
)

type PayHandler struct {
	renderer      *Renderer
	tariffs       *models.TariffStore
	receipts      *models.PaymentReceiptStore
	users         *models.UserStore
	mailer        *email.Sender
	payServiceURL string
	panelBaseURL  string
	webhookSecret string
}

// webhookPayload зеркалит payment.WebhookPayload из pay-service.
type webhookPayload struct {
	InvID       int             `json:"inv_id"`
	ProductType string          `json:"product_type"`
	PlanID      string          `json:"plan_id"`
	Amount      float64         `json:"amount"`
	Status      string          `json:"status"`
	UserRef     string          `json:"user_ref"`
	Email       string          `json:"email"`
	Metadata    json.RawMessage `json:"metadata"`
	PaidAt      string          `json:"paid_at"`
}

func NewPayHandler(
	renderer *Renderer,
	tariffs *models.TariffStore,
	receipts *models.PaymentReceiptStore,
	users *models.UserStore,
	mailer *email.Sender,
	payServiceURL, panelBaseURL, webhookSecret string,
) *PayHandler {
	return &PayHandler{
		renderer:      renderer,
		tariffs:       tariffs,
		receipts:      receipts,
		users:         users,
		mailer:        mailer,
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

// Webhook — приёмник вебхука от pay-service.
// POST /api/payments/webhook
// Заголовок: X-Webhook-Signature: hex(hmac-sha256(body, WEBHOOK_SECRET))
// Ответ 200 OK — pay-service прекращает ретраи.
func (h *PayHandler) Webhook(w http.ResponseWriter, r *http.Request) {
	if h.webhookSecret == "" {
		log.Printf("Webhook: WEBHOOK_SECRET not configured")
		http.Error(w, "webhook disabled", http.StatusServiceUnavailable)
		return
	}

	// 1. Читаем raw body — подпись считается от байтов ДО парсинга JSON
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10)) // 64 KiB
	if err != nil {
		log.Printf("Webhook: read body error: %v", err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// 2. Проверяем HMAC-SHA256
	signature := r.Header.Get("X-Webhook-Signature")
	if signature == "" || !paysign.VerifyBody(body, signature, h.webhookSecret) {
		log.Printf("Webhook: invalid signature from %s", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// 3. Парсим payload
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		log.Printf("Webhook: json decode error: %v", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// 4. Фильтруем неоплаченные статусы — это не ошибка, просто игнор
	if p.Status != "paid" {
		log.Printf("Webhook: inv_id=%d status=%s — skipped", p.InvID, p.Status)
		writeOK(w)
		return
	}

	// 5. Только свой product_type
	if p.ProductType != "vpn" {
		log.Printf("Webhook: inv_id=%d unknown product_type=%q — skipped",
			p.InvID, p.ProductType)
		writeOK(w) // OK, чтобы pay-service не ретраил
		return
	}

	// 6. user_ref → user_id
	userID, err := strconv.Atoi(p.UserRef)
	if err != nil || userID <= 0 {
		log.Printf("Webhook: inv_id=%d invalid user_ref=%q", p.InvID, p.UserRef)
		// 400 — данные сломаны, ретраить бессмысленно, но pay-service поретраит
		// и через 3 попытки перестанет. Лучше, чем молча потерять платёж.
		http.Error(w, "invalid user_ref", http.StatusBadRequest)
		return
	}

	// 7. Парсим metadata — там traffic_gb, зафиксированный на момент checkout
	var meta struct {
		TrafficGB float64 `json:"traffic_gb"`
	}
	if len(p.Metadata) > 0 {
		if err := json.Unmarshal(p.Metadata, &meta); err != nil {
			log.Printf("Webhook: inv_id=%d metadata parse error: %v", p.InvID, err)
		}
	}

	// 8. Sanity-check: тариф должен существовать (защита от мусорных plan_id)
	tariff, err := h.tariffs.GetByCode(r.Context(), p.PlanID)
	if err != nil {
		log.Printf("Webhook: inv_id=%d unknown plan_id=%q: %v", p.InvID, p.PlanID, err)
		http.Error(w, "unknown plan", http.StatusBadRequest)
		return
	}

	// 9. Определяем, сколько ГБ начислить. Приоритет: metadata → тариф (fallback).
	trafficGB := meta.TrafficGB
	if trafficGB <= 0 {
		log.Printf("Webhook: inv_id=%d metadata.traffic_gb missing, fallback to tariff %q",
			p.InvID, tariff.Code)
		trafficGB = tariff.TrafficGB
	}
	if trafficGB <= 0 {
		log.Printf("Webhook: inv_id=%d traffic_gb not determined", p.InvID)
		http.Error(w, "invalid traffic amount", http.StatusBadRequest)
		return
	}

	// 10. Разумный диапазон — защита от JSON-сюрпризов (0.1 … 10000 ГБ)
	if trafficGB < 0.1 || trafficGB > 10000 {
		log.Printf("Webhook: inv_id=%d traffic_gb=%.2f out of range", p.InvID, trafficGB)
		http.Error(w, "invalid traffic amount", http.StatusBadRequest)
		return
	}

	// 11. Сумма — всегда из payload (это что реально списалось)
	amountRub := p.Amount
	if amountRub <= 0 {
		log.Printf("Webhook: inv_id=%d invalid amount=%.2f", p.InvID, amountRub)
		http.Error(w, "invalid amount", http.StatusBadRequest)
		return
	}

	trafficBytes := int64(trafficGB * 1024 * 1024 * 1024)

	paidAt, err := time.Parse(time.RFC3339, p.PaidAt)
	if err != nil {
		paidAt = time.Now()
	}

	// 12. Атомарно: квитанция + баланс
	receipt := &models.PaymentReceipt{
		InvID:        p.InvID,
		UserID:       userID,
		PlanID:       tariff.Code,
		AmountRub:    amountRub,    // ← из payload
		TrafficBytes: trafficBytes, // ← из metadata
		PaidAt:       paidAt,
		RawPayload:   body,
	}

	err = h.receipts.CreditBalance(r.Context(), receipt)
	switch {
	case errors.Is(err, models.ErrReceiptExists):
		// Идемпотентный повтор — это нормально, просто OK
		log.Printf("Webhook: inv_id=%d already processed (idempotent)", p.InvID)
		writeOK(w)
		return
	case err != nil:
		log.Printf("Webhook: inv_id=%d credit error: %v", p.InvID, err)
		// 500 → pay-service поретраит через 2/5/10 сек — шанс восстановиться
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("Webhook: inv_id=%d PAID — user=%d plan=%s +%.1f GB (%.2f ₽)",
		p.InvID, userID, tariff.Code, trafficGB, amountRub)

	if h.mailer != nil {
		go h.sendTopupEmail(userID, p.InvID, trafficGB, amountRub)
	}

	writeOK(w)
}

func (h *PayHandler) sendTopupEmail(userID, invID int, trafficGB, amountRub float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	u, err := h.users.GetByID(ctx, userID)
	if err != nil {
		log.Printf("Topup mail: get user %d failed: %v", userID, err)
		return
	}

	if err := h.mailer.SendTopupNotification(
		u.Email, u.Username, trafficGB, amountRub, invID, h.panelBaseURL,
	); err != nil {
		log.Printf("Topup mail: send to %s failed: %v", u.Email, err)
		return
	}
	log.Printf("Topup mail: sent to %s (user %d, inv_id=%d)", u.Email, userID, invID)
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
