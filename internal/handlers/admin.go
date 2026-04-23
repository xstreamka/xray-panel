package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
	"xray-panel/internal/xray"

	"github.com/go-chi/chi/v5"
)

type AdminHandler struct {
	users      *models.UserStore
	profiles   *models.VPNProfileStore
	tariffs    *models.TariffStore
	xrayHolder *xray.Holder
	renderer   *Renderer
}

func NewAdminHandler(
	users *models.UserStore,
	profiles *models.VPNProfileStore,
	tariffs *models.TariffStore,
	xrayHolder *xray.Holder,
	renderer *Renderer,
) *AdminHandler {
	return &AdminHandler{
		users: users, profiles: profiles, tariffs: tariffs,
		xrayHolder: xrayHolder, renderer: renderer,
	}
}

// enrichAllProfiles добавляет live-трафик и онлайн-статус ко всем профилям
func (h *AdminHandler) enrichAllProfiles(ctx context.Context, profiles []models.VPNProfile) (
	enriched []models.VPNProfile,
	onlineUsers map[string]bool,
	onlineIPs map[string][]string,
) {
	enriched = profiles
	onlineUsers = make(map[string]bool)

	if collector := h.xrayHolder.GetCollector(); collector != nil {
		liveTraffic, online, ips := collector.Snapshot()
		if liveTraffic != nil {
			for i, p := range enriched {
				if stats, ok := liveTraffic[p.UUID]; ok {
					enriched[i].TrafficUp += stats[0]
					enriched[i].TrafficDown += stats[1]
				}
			}
			onlineUsers = online
			onlineIPs = ips
			return
		}
	}

	if client := h.xrayHolder.Get(); client != nil {
		var liveTraffic map[string][2]int64
		if lt, err := client.QueryAllUserTraffic(ctx, false); err == nil {
			liveTraffic = lt
			for i, p := range enriched {
				if stats, ok := liveTraffic[p.UUID]; ok {
					enriched[i].TrafficUp += stats[0]
					enriched[i].TrafficDown += stats[1]
				}
			}
		}
		if online, err := client.GetOnlineUsers(ctx, liveTraffic); err == nil {
			onlineUsers = online
		}
		onlineIPs = client.GetOnlineIPs(ctx, onlineUsers)
	}

	return
}

func (h *AdminHandler) Users(w http.ResponseWriter, r *http.Request) {
	users, err := h.users.List(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	profiles, err := h.profiles.ListAll(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	profiles, onlineUsers, onlineIPs := h.enrichAllProfiles(r.Context(), profiles)

	// Подписочные тарифы — для селекта «задать подписку» в каждой карточке юзера
	subPlans, err := h.tariffs.ListActiveByKind(r.Context(), models.TariffKindSubscription)
	if err != nil {
		log.Printf("Admin: list sub tariffs error: %v", err)
	}

	// Имена тарифов для отображения текущей подписки юзера
	tariffNames := make(map[int]string)
	for _, t := range subPlans {
		tariffNames[t.ID] = t.Label
	}

	type profileView struct {
		models.VPNProfile
		IsOnline  bool
		OnlineIPs []string
	}

	profilesByUser := make(map[int][]profileView)
	for _, p := range profiles {
		pv := profileView{
			VPNProfile: p,
			IsOnline:   p.IsActive && onlineUsers[p.UUID],
			OnlineIPs:  onlineIPs[p.UUID],
		}
		profilesByUser[p.UserID] = append(profilesByUser[p.UserID], pv)
	}

	type userView struct {
		models.User
		Profiles     []profileView
		TotalTraffic int64
		ActiveCount  int
		OnlineCount  int
		TariffLabel  string
		DaysLeft     int
	}

	var views []userView
	for _, u := range users {
		v := userView{User: u, Profiles: profilesByUser[u.ID]}
		for _, p := range v.Profiles {
			v.TotalTraffic += p.TrafficUp + p.TrafficDown
			if p.IsActive {
				v.ActiveCount++
			}
			if p.IsOnline {
				v.OnlineCount++
			}
		}

		if u.CurrentTariffID != nil {
			if name, ok := tariffNames[*u.CurrentTariffID]; ok {
				v.TariffLabel = name
			} else if t, err := h.tariffs.GetByID(r.Context(), *u.CurrentTariffID); err == nil {
				v.TariffLabel = t.Label
			}
		}
		if u.TariffExpiresAt != nil {
			d := time.Until(*u.TariffExpiresAt).Hours() / 24
			if d > 0 {
				v.DaysLeft = int(d) + 1
			}
		}

		views = append(views, v)
	}

	h.renderer.Render(w, "admin.html", map[string]any{
		"Active":   "admin",
		"User":     middleware.UserFromContext(r.Context()),
		"Users":    views,
		"SubPlans": subPlans,
	})
}

func (h *AdminHandler) ToggleProfile(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	action := r.FormValue("action")

	profile, err := h.profiles.GetByID(r.Context(), id)
	if err != nil {
		http.Error(w, "Профиль не найден", http.StatusNotFound)
		return
	}

	switch action {
	case "activate":
		if fw := h.xrayHolder.GetFirewall(); fw != nil {
			fw.UnblockUser(profile.UUID)
		}
		if err := h.profiles.SetActive(r.Context(), id, true); err != nil {
			log.Printf("Admin: activate error: %v", err)
		}
		if client := h.xrayHolder.Get(); client != nil {
			if err := client.AddUser(r.Context(), profile.UUID, profile.UUID); err != nil {
				log.Printf("Admin: xray add error: %v", err)
			}
		}
		if collector := h.xrayHolder.GetCollector(); collector != nil {
			collector.RegisterProfile(profile.UUID, profile.UserID, profile.TrafficLimit)
		}
		log.Printf("Admin: profile %s activated", profile.UUID)

	case "deactivate":
		if err := h.profiles.SetActive(r.Context(), id, false); err != nil {
			log.Printf("Admin: deactivate error: %v", err)
		}
		if client := h.xrayHolder.Get(); client != nil {
			if fw := h.xrayHolder.GetFirewall(); fw != nil {
				fw.BlockUser(r.Context(), client, profile.UUID)
			}
			if err := client.RemoveUser(r.Context(), profile.UUID); err != nil {
				log.Printf("Admin: xray remove error: %v", err)
			}
		}
		log.Printf("Admin: profile %s deactivated", profile.UUID)
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *AdminHandler) SetLimit(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	limitGB, _ := strconv.ParseFloat(r.FormValue("limit_gb"), 64)

	limitBytes := int64(limitGB * 1024 * 1024 * 1024)

	if err := h.profiles.SetLimit(r.Context(), id, limitBytes); err != nil {
		log.Printf("Admin: set limit error: %v", err)
	}

	profile, err := h.profiles.GetByID(r.Context(), id)
	if err == nil {
		if collector := h.xrayHolder.GetCollector(); collector != nil {
			collector.UpdateLimit(profile.UUID, limitBytes)
		}
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *AdminHandler) ResetTraffic(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))

	profile, err := h.profiles.GetByID(r.Context(), id)
	if err != nil {
		http.Error(w, "Профиль не найден", http.StatusNotFound)
		return
	}

	if err := h.profiles.ResetTraffic(r.Context(), id); err != nil {
		log.Printf("Admin: reset traffic error: %v", err)
	}

	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.ResetCumulative(profile.UUID)
	}

	if !profile.IsActive {
		if fw := h.xrayHolder.GetFirewall(); fw != nil {
			fw.UnblockUser(profile.UUID)
		}
		h.profiles.SetActive(r.Context(), id, true)
		if client := h.xrayHolder.Get(); client != nil {
			client.AddUser(r.Context(), profile.UUID, profile.UUID)
		}
		if collector := h.xrayHolder.GetCollector(); collector != nil {
			collector.RegisterProfile(profile.UUID, profile.UserID, profile.TrafficLimit)
		}
		log.Printf("Admin: profile %s reactivated after traffic reset", profile.UUID)
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// SetExtraBalance — POST /admin/users/{id}/extra — установить extra-баланс
// в ровно заданное значение (ГБ). Используется для ручной коррекции: и для
// добавления, и для списания, и для сброса в 0.
func (h *AdminHandler) SetExtraBalance(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.Atoi(chi.URLParam(r, "id"))
	gb, err := strconv.ParseFloat(r.FormValue("extra_gb"), 64)
	if err != nil || gb < 0 {
		http.Error(w, "Укажите количество ГБ ≥ 0", http.StatusBadRequest)
		return
	}

	bytes := int64(gb * 1024 * 1024 * 1024)

	if err := h.users.SetExtra(r.Context(), userID, bytes); err != nil {
		log.Printf("Admin: set extra error for user %d: %v", userID, err)
		http.Error(w, "Ошибка сохранения", http.StatusInternalServerError)
		return
	}

	// Если юзер был отключён из-за исчерпанного баланса — вернём профили
	// в Xray. Если не был — это no-op.
	if bytes > 0 {
		if collector := h.xrayHolder.GetCollector(); collector != nil {
			collector.ReactivateUserAll(r.Context(), userID)
		}
	}

	log.Printf("Admin: set extra=%.2f GB for user %d", gb, userID)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// SetSubscription — POST /admin/users/{id}/subscription — активировать/продлить
// подписку юзера указанным тарифом. Если duration_days не задан или <=0, берётся
// из тарифа. Логика продления — как в штатном webhook'е: max(NOW(), old_expires).
func (h *AdminHandler) SetSubscription(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.Atoi(chi.URLParam(r, "id"))
	tariffID, _ := strconv.Atoi(r.FormValue("tariff_id"))
	durationDays, _ := strconv.Atoi(r.FormValue("duration_days"))

	if tariffID <= 0 {
		http.Error(w, "Не выбран тариф", http.StatusBadRequest)
		return
	}

	tariff, err := h.tariffs.GetByID(r.Context(), tariffID)
	if err != nil {
		http.Error(w, "Тариф не найден", http.StatusBadRequest)
		return
	}
	if tariff.Kind != models.TariffKindSubscription {
		http.Error(w, "Нельзя активировать подписку по тарифу-докупке", http.StatusBadRequest)
		return
	}

	if durationDays <= 0 {
		durationDays = tariff.DurationDays
	}
	if durationDays <= 0 {
		http.Error(w, "Некорректный срок подписки", http.StatusBadRequest)
		return
	}

	baseLimitBytes := int64(tariff.TrafficGB * 1024 * 1024 * 1024)

	if _, err := h.users.RenewSubscription(r.Context(), userID, tariffID, durationDays, baseLimitBytes); err != nil {
		log.Printf("Admin: RenewSubscription user=%d tariff=%d: %v", userID, tariffID, err)
		http.Error(w, "Ошибка активации подписки", http.StatusInternalServerError)
		return
	}

	// Если у юзера были отключены профили (баланс был 0 / подписка истекла) —
	// возвращаем их в Xray. Для уже активных юзеров метод — no-op.
	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.ReactivateUserAll(r.Context(), userID)
	}

	log.Printf("Admin: activated subscription user=%d tariff=%s duration=%d base=%.1f GB",
		userID, tariff.Code, durationDays, tariff.TrafficGB)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ToggleUserActive — POST /admin/users/{id}/toggle — включить/выключить
// учётную запись. При выключении немедленно рвём сессию (на следующем запросе
// юзера выкинет middleware) и отключаем все его VPN-профили через Xray —
// чтобы уже подключённые клиенты не продолжали жить до перезапуска.
// При включении профили назад не поднимаем: если нужно — админ делает это
// кнопкой на конкретном профиле.
func (h *AdminHandler) ToggleUserActive(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.Atoi(chi.URLParam(r, "id"))
	action := r.FormValue("action")

	switch action {
	case "activate":
		if err := h.users.SetActive(r.Context(), userID, true); err != nil {
			log.Printf("Admin: SetActive(true) user=%d: %v", userID, err)
			http.Error(w, "Ошибка активации", http.StatusInternalServerError)
			return
		}
		log.Printf("Admin: user %d activated", userID)

	case "deactivate":
		if err := h.users.SetActive(r.Context(), userID, false); err != nil {
			log.Printf("Admin: SetActive(false) user=%d: %v", userID, err)
			http.Error(w, "Ошибка деактивации", http.StatusInternalServerError)
			return
		}
		if collector := h.xrayHolder.GetCollector(); collector != nil {
			collector.DisconnectUserAll(r.Context(), userID, "user deactivated by admin")
		}
		log.Printf("Admin: user %d deactivated", userID)

	default:
		http.Error(w, "Неизвестное действие", http.StatusBadRequest)
		return
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// CancelSubscription — POST /admin/users/{id}/subscription/cancel
// Обнуляет подписку (current_tariff_id, tariff_expires_at, base_limit, base_used).
// extra и frozen не трогает — админ может отдельно списать, если нужно.
// Профили не отключаем принудительно: если у юзера остался extra, он имеет
// право его использовать; коллектор сам отключит при TotalAvailable=0.
func (h *AdminHandler) CancelSubscription(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.Atoi(chi.URLParam(r, "id"))

	if err := h.users.CancelSubscription(r.Context(), userID); err != nil {
		log.Printf("Admin: CancelSubscription user=%d: %v", userID, err)
		http.Error(w, "Ошибка отмены", http.StatusInternalServerError)
		return
	}

	log.Printf("Admin: cancelled subscription user=%d", userID)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

type adminProfileStatsJSON struct {
	ID          int      `json:"id"`
	IsOnline    bool     `json:"is_online"`
	OnlineIPs   []string `json:"online_ips"`
	TrafficUp   string   `json:"traffic_up_fmt"`
	TrafficDown string   `json:"traffic_down_fmt"`
}

type adminUserStatsJSON struct {
	UserID      int                     `json:"user_id"`
	OnlineCount int                     `json:"online_count"`
	Total       string                  `json:"total_traffic_fmt"`
	BalanceFmt  string                  `json:"balance_fmt"`
	Profiles    []adminProfileStatsJSON `json:"profiles"`
}

func (h *AdminHandler) StatsJSON(w http.ResponseWriter, r *http.Request) {
	users, err := h.users.List(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	profiles, err := h.profiles.ListAll(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	profiles, onlineUsers, onlineIPs := h.enrichAllProfiles(r.Context(), profiles)

	type profByUser struct {
		models.VPNProfile
		IsOnline  bool
		OnlineIPs []string
	}
	profilesByUser := make(map[int][]profByUser)
	for _, p := range profiles {
		profilesByUser[p.UserID] = append(profilesByUser[p.UserID], profByUser{
			VPNProfile: p,
			IsOnline:   p.IsActive && onlineUsers[p.UUID],
			OnlineIPs:  onlineIPs[p.UUID],
		})
	}

	result := make([]adminUserStatsJSON, 0, len(users))
	for _, u := range users {
		uv := adminUserStatsJSON{
			UserID:     u.ID,
			BalanceFmt: formatBytesGo(u.TotalAvailable()),
		}
		var totalTraffic int64
		for _, p := range profilesByUser[u.ID] {
			totalTraffic += p.TrafficUp + p.TrafficDown
			if p.IsOnline {
				uv.OnlineCount++
			}
			uv.Profiles = append(uv.Profiles, adminProfileStatsJSON{
				ID:          p.ID,
				IsOnline:    p.IsOnline,
				OnlineIPs:   p.OnlineIPs,
				TrafficUp:   formatBytesGo(p.TrafficUp),
				TrafficDown: formatBytesGo(p.TrafficDown),
			})
		}
		uv.Total = formatBytesGo(totalTraffic)
		result = append(result, uv)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ──────────────────────────────────────────────
// Тарифы
// ──────────────────────────────────────────────

func (h *AdminHandler) TariffsList(w http.ResponseWriter, r *http.Request) {
	tariffs, err := h.tariffs.ListAll(r.Context())
	if err != nil {
		log.Printf("Admin: list tariffs error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.renderer.Render(w, "tariffs.html", map[string]any{
		"Active":  "admin-tariffs",
		"User":    middleware.UserFromContext(r.Context()),
		"Tariffs": tariffs,
	})
}

func (h *AdminHandler) TariffCreate(w http.ResponseWriter, r *http.Request) {
	t := parseTariffForm(r)
	if err := validateTariff(t); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.tariffs.Create(r.Context(), t); err != nil {
		log.Printf("Admin: tariff create error: %v", err)
		http.Error(w, "Ошибка создания тарифа: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Admin: tariff created code=%s amount=%.2f traffic=%.1f",
		t.Code, t.AmountRub, t.TrafficGB)
	http.Redirect(w, r, "/admin/tariffs", http.StatusSeeOther)
}

func (h *AdminHandler) TariffUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	t := parseTariffForm(r)
	t.ID = id
	if err := validateTariff(t); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.tariffs.Update(r.Context(), t); err != nil {
		log.Printf("Admin: tariff update error: %v", err)
		http.Error(w, "Ошибка обновления: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/tariffs", http.StatusSeeOther)
}

func (h *AdminHandler) TariffDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	if err := h.tariffs.Delete(r.Context(), id); err != nil {
		log.Printf("Admin: tariff delete error: %v", err)
		http.Error(w, "Ошибка удаления", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/tariffs", http.StatusSeeOther)
}

func parseTariffForm(r *http.Request) *models.Tariff {
	amount, _ := strconv.ParseFloat(r.FormValue("amount_rub"), 64)
	gb, _ := strconv.ParseFloat(r.FormValue("traffic_gb"), 64)
	sortOrder, _ := strconv.Atoi(r.FormValue("sort_order"))
	durationDays, _ := strconv.Atoi(r.FormValue("duration_days"))

	kind := models.TariffKind(strings.TrimSpace(r.FormValue("kind")))
	if kind == "" {
		kind = models.TariffKindSubscription
	}
	// Для addon длительность игнорируется на уровне БД-констрейнта,
	// но для чистоты обнулим — чтобы UI/админка не вводили в заблуждение.
	if kind == models.TariffKindAddon {
		durationDays = 0
	}

	return &models.Tariff{
		Code:         strings.TrimSpace(r.FormValue("code")),
		Label:        strings.TrimSpace(r.FormValue("label")),
		Description:  strings.TrimSpace(r.FormValue("description")),
		AmountRub:    amount,
		TrafficGB:    gb,
		Kind:         kind,
		DurationDays: durationDays,
		IsPopular:    r.FormValue("is_popular") == "on",
		IsActive:     r.FormValue("is_active") == "on",
		SortOrder:    sortOrder,
	}
}

func validateTariff(t *models.Tariff) error {
	if t.Code == "" || t.Label == "" || t.Description == "" {
		return fmt.Errorf("заполните код, название и описание")
	}
	if t.AmountRub <= 0 {
		return fmt.Errorf("цена должна быть больше нуля")
	}
	if t.TrafficGB <= 0 {
		return fmt.Errorf("количество ГБ должно быть больше нуля")
	}
	switch t.Kind {
	case models.TariffKindSubscription:
		if t.DurationDays <= 0 {
			return fmt.Errorf("для подписки задайте срок в днях (> 0)")
		}
	case models.TariffKindAddon:
		// ok
	default:
		return fmt.Errorf("недопустимый тип тарифа: %q", t.Kind)
	}
	// Description уходит в Робокассу. Допустимы латиница, кириллица, цифры,
	// пробел и базовые знаки препинания. Длина — максимум 100 символов.
	if utf8.RuneCountInString(t.Description) > 100 {
		return fmt.Errorf("описание не длиннее 100 символов")
	}
	for _, r := range t.Description {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r >= 'а' && r <= 'я',
			r >= 'А' && r <= 'Я',
			r == 'ё', r == 'Ё',
			r == ' ', r == '.', r == ',', r == '!', r == '?',
			r == '-', r == '(', r == ')', r == ':', r == ';':
			// ок
		default:
			return fmt.Errorf("недопустимый символ в описании: %q (разрешены буквы, цифры и базовая пунктуация: . , ! ? - ( ) : ;)", r)
		}
	}
	return nil
}
