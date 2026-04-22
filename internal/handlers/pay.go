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
	"xray-panel/internal/xray"
)

type PayHandler struct {
	renderer      *Renderer
	tariffs       *models.TariffStore
	receipts      *models.PaymentReceiptStore
	users         *models.UserStore
	mailer        *email.Sender
	xrayHolder    *xray.Holder
	payServiceURL string
	panelBaseURL  string
	webhookSecret string
}

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
	xrayHolder *xray.Holder,
	payServiceURL, panelBaseURL, webhookSecret string,
) *PayHandler {
	return &PayHandler{
		renderer:      renderer,
		tariffs:       tariffs,
		receipts:      receipts,
		users:         users,
		mailer:        mailer,
		xrayHolder:    xrayHolder,
		payServiceURL: strings.TrimRight(payServiceURL, "/"),
		panelBaseURL:  strings.TrimRight(panelBaseURL, "/"),
		webhookSecret: webhookSecret,
	}
}

func (h *PayHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	if h.payServiceURL == "" || h.webhookSecret == "" {
		http.Error(w, "Оплата временно недоступна: PAY_SERVICE_URL или WEBHOOK_SECRET не настроены",
			http.StatusServiceUnavailable)
		return
	}

	// Свежий юзер — для корректной проверки HasActiveSubscription()
	freshUser, err := h.users.GetByID(r.Context(), user.ID)
	if err == nil {
		user = freshUser
	}

	subPlans, err := h.tariffs.ListActiveByKind(r.Context(), models.TariffKindSubscription)
	if err != nil {
		log.Printf("Pay: list sub tariffs error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	addonPlans, err := h.tariffs.ListActiveByKind(r.Context(), models.TariffKindAddon)
	if err != nil {
		log.Printf("Pay: list addon tariffs error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Для обратной совместимости со старым шаблоном (до UI-рефакторинга в шаге 6)
	// отдаём также плоский список всех активных тарифов.
	allPlans := append(append([]models.Tariff{}, subPlans...), addonPlans...)

	h.renderer.Render(w, "pay.html", map[string]any{
		"User":         user,
		"Tariffs":      allPlans, // legacy ключ
		"SubPlans":     subPlans,
		"AddonPlans":   addonPlans,
		"HasActiveSub": user.HasActiveSubscription(),
		"Status":       r.URL.Query().Get("payment"),
		"InvID":        r.URL.Query().Get("inv_id"),
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

	// Addon можно покупать ТОЛЬКО при активной подписке. Иначе неясно,
	// куда класть трафик — extra без подписки всё равно уйдёт во frozen
	// при первом же expire-тике, а новой подписки нет. Защищаем юзера.
	if tariff.Kind == models.TariffKindAddon {
		fresh, err := h.users.GetByID(r.Context(), user.ID)
		if err != nil || !fresh.HasActiveSubscription() {
			http.Error(w,
				"Докупка трафика доступна только при активной подписке. "+
					"Сначала оплатите тарифный план.",
				http.StatusBadRequest)
			return
		}
	}

	// В metadata теперь кладём больше контекста. Webhook в первую очередь
	// смотрит в БД (через GetByCode), но эти поля страхуют кейс, когда тариф
	// был изменён/удалён между checkout и webhook.
	metadata, _ := json.Marshal(map[string]any{
		"traffic_gb":    tariff.TrafficGB,
		"duration_days": tariff.DurationDays,
		"kind":          string(tariff.Kind),
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
	log.Printf("Pay: user %d (%s) → checkout %s [%s] (%.2f ₽)",
		user.ID, user.Email, tariff.Code, tariff.Kind, tariff.AmountRub)

	http.Redirect(w, r, checkoutURL, http.StatusSeeOther)
}

// Webhook — приёмник вебхука от pay-service.
// POST /api/payments/webhook
func (h *PayHandler) Webhook(w http.ResponseWriter, r *http.Request) {
	if h.webhookSecret == "" {
		log.Printf("Webhook: WEBHOOK_SECRET not configured")
		http.Error(w, "webhook disabled", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		log.Printf("Webhook: read body error: %v", err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	signature := r.Header.Get("X-Webhook-Signature")
	if signature == "" || !paysign.VerifyBody(body, signature, h.webhookSecret) {
		log.Printf("Webhook: invalid signature from %s", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		log.Printf("Webhook: json decode error: %v", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if p.Status != "paid" {
		log.Printf("Webhook: inv_id=%d status=%s — skipped", p.InvID, p.Status)
		writeOK(w)
		return
	}

	if p.ProductType != "vpn" {
		log.Printf("Webhook: inv_id=%d unknown product_type=%q — skipped",
			p.InvID, p.ProductType)
		writeOK(w)
		return
	}

	userID, err := strconv.Atoi(p.UserRef)
	if err != nil || userID <= 0 {
		log.Printf("Webhook: inv_id=%d invalid user_ref=%q", p.InvID, p.UserRef)
		http.Error(w, "invalid user_ref", http.StatusBadRequest)
		return
	}

	var meta struct {
		TrafficGB float64 `json:"traffic_gb"`
	}
	if len(p.Metadata) > 0 {
		if err := json.Unmarshal(p.Metadata, &meta); err != nil {
			log.Printf("Webhook: inv_id=%d metadata parse error: %v", p.InvID, err)
		}
	}

	tariff, err := h.tariffs.GetByCode(r.Context(), p.PlanID)
	if err != nil {
		log.Printf("Webhook: inv_id=%d unknown plan_id=%q: %v", p.InvID, p.PlanID, err)
		http.Error(w, "unknown plan", http.StatusBadRequest)
		return
	}

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
	if trafficGB < 0.1 || trafficGB > 10000 {
		log.Printf("Webhook: inv_id=%d traffic_gb=%.2f out of range", p.InvID, trafficGB)
		http.Error(w, "invalid traffic amount", http.StatusBadRequest)
		return
	}

	// Повторная валидация для addon: если тариф аддоновский, у юзера на
	// момент ОПЛАТЫ должна быть активная подписка. Защита от редкого кейса,
	// когда подписка истекла между checkout и оплатой.
	if tariff.Kind == models.TariffKindAddon {
		u, err := h.users.GetByID(r.Context(), userID)
		if err != nil {
			log.Printf("Webhook: inv_id=%d user %d not found: %v", p.InvID, userID, err)
			http.Error(w, "user not found", http.StatusBadRequest)
			return
		}
		if !u.HasActiveSubscription() {
			// Не 400! Это оплаченный платёж — нужно его учесть как обычное
			// пополнение extra. ApplyPayment это сделает в ветке addon.
			// Юзер получит трафик, но extra может уйти в frozen при ближайшем
			// истечении. В логе — warning, чтобы можно было расследовать.
			log.Printf("Webhook: inv_id=%d WARN addon paid by user %d without active subscription",
				p.InvID, userID)
		}
	}

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

	receipt := &models.PaymentReceipt{
		InvID:        p.InvID,
		UserID:       userID,
		PlanID:       tariff.Code,
		AmountRub:    amountRub,
		TrafficBytes: trafficBytes,
		PaidAt:       paidAt,
		RawPayload:   body,
	}

	// ApplyPayment сам выбирает ветку по tariff.Kind:
	//   subscription → продление/активация + сброс base + разморозка extra
	//   addon        → просто +extra_traffic_balance
	// Всё атомарно в одной транзакции.
	err = h.receipts.ApplyPayment(r.Context(), receipt, tariff)
	switch {
	case errors.Is(err, models.ErrReceiptExists):
		log.Printf("Webhook: inv_id=%d already processed (idempotent)", p.InvID)
		writeOK(w)
		return
	case err != nil:
		log.Printf("Webhook: inv_id=%d apply error: %v", p.InvID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("Webhook: inv_id=%d PAID — user=%d plan=%s kind=%s +%.1f GB (%.2f ₽)",
		p.InvID, userID, tariff.Code, tariff.Kind, trafficGB, amountRub)

	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.ReactivateUserAll(r.Context(), userID)
	}

	if h.mailer != nil {
		go h.sendPaymentEmail(userID, p.InvID, tariff, trafficGB, amountRub)
	}

	writeOK(w)
}

func (h *PayHandler) sendPaymentEmail(
	userID, invID int, tariff *models.Tariff,
	trafficGB, amountRub float64,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	u, err := h.users.GetByID(ctx, userID)
	if err != nil {
		log.Printf("Payment mail: get user %d failed: %v", userID, err)
		return
	}

	// TODO(шаг 6 / email): заменить на отдельные SendSubscriptionNotification /
	// SendAddonNotification с разными текстами письма. Пока используем старый
	// шаблон — он корректно расскажет про начисленные ГБ и сумму, но не
	// упомянет срок подписки. Для MVP этого достаточно.
	if err := h.mailer.SendTopupNotification(
		u.Email, u.Username, trafficGB, amountRub, invID, h.panelBaseURL,
	); err != nil {
		log.Printf("Payment mail: send to %s failed: %v", u.Email, err)
		return
	}
	log.Printf("Payment mail: sent to %s (user %d, inv_id=%d, kind=%s)",
		u.Email, userID, invID, tariff.Kind)
}

func (h *PayHandler) History(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	receipts, err := h.receipts.ListByUser(r.Context(), user.ID, 100)
	if err != nil {
		log.Printf("Pay history: list error for user %d: %v", user.ID, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	h.renderer.Render(w, "payments_history.html", map[string]any{
		"User":     user,
		"Receipts": receipts,
	})
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
