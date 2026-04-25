package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
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
	users       *models.UserStore
	profiles    *models.VPNProfileStore
	tariffs     *models.TariffStore
	invites     *models.InviteStore
	trafficLogs *models.TrafficLogStore
	xrayHolder  *xray.Holder
	renderer    *Renderer
	baseURL     string
}

func NewAdminHandler(
	users *models.UserStore,
	profiles *models.VPNProfileStore,
	tariffs *models.TariffStore,
	invites *models.InviteStore,
	trafficLogs *models.TrafficLogStore,
	xrayHolder *xray.Holder,
	renderer *Renderer,
	baseURL string,
) *AdminHandler {
	return &AdminHandler{
		users: users, profiles: profiles, tariffs: tariffs, invites: invites,
		trafficLogs: trafficLogs,
		xrayHolder:  xrayHolder, renderer: renderer, baseURL: baseURL,
	}
}

// adminProfileView — строчка профиля с живыми данными (онлайн + IP).
// UsagePercent/ProgressColor заполняются только если TrafficLimit > 0
// (личный лимит профиля). Без лимита прогресс-бар не рисуется — см. шаблон.
type adminProfileView struct {
	models.VPNProfile
	IsOnline      bool
	OnlineIPs     []string
	TrafficTotal  int64 // up + down, чтобы не считать в шаблоне
	UsagePercent  int
	ProgressColor string
}

// adminUserView — пользователь + агрегаты + профили. Один общий тип для
// списка /admin и детальной карточки /admin/users/{id}, чтобы шаблоны
// обращались к одним и тем же полям. Поля Base/ExtraPercent и ExtraUsed —
// для прогресс-баров на карточке (в списке не используются).
type adminUserView struct {
	models.User
	Profiles     []adminProfileView
	TotalTraffic int64
	ActiveCount  int
	OnlineCount  int
	TariffLabel  string
	DaysLeft     int
	BasePercent  int
	ExtraUsed    int64
	ExtraPercent int
}

// progressColor — цветовая шкала для прогресс-бара. Совпадает с dashboard:
// < 70% зелёный, 70–90% оранжевый, ≥ 90% красный. Для extra — желтый
// базовый цвет (см. шаблон, передаётся отдельно).
func progressColor(pct int) string {
	switch {
	case pct >= 90:
		return "#ef4444"
	case pct >= 70:
		return "#f59e0b"
	default:
		return "#22c55e"
	}
}

// adminRedirectBack — куда слать юзера после POST-действия.
//   - явный return_to из формы, если он локальный (начинается с "/")
//   - иначе Referer, если локальный
//   - иначе /admin
//
// Нужно, чтобы действия, выполненные с карточки /admin/users/{id}, возвращали
// на эту же карточку, а действия со списка /admin — на список.
func adminRedirectBack(r *http.Request) string {
	rt := r.FormValue("return_to")
	if isLocalAdminPath(rt) {
		return rt
	}
	if ref := r.Referer(); ref != "" {
		// Берём только path — Referer приходит полным URL.
		if u, err := urlParseRef(ref); err == nil && isLocalAdminPath(u) {
			return u
		}
	}
	return "/admin"
}

// isLocalAdminPath защищает от open-redirect: принимаем только пути вида
// "/admin..." и без схемы/хоста.
func isLocalAdminPath(p string) bool {
	if p == "" || !strings.HasPrefix(p, "/admin") {
		return false
	}
	if strings.Contains(p, "://") || strings.HasPrefix(p, "//") {
		return false
	}
	return true
}

// urlParseRef вытаскивает Path из абсолютного URL Referer'а.
func urlParseRef(ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	if u.Path == "" {
		return "", fmt.Errorf("empty path")
	}
	return u.Path, nil
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

// buildUserView собирает полный view для одного юзера: агрегаты по профилям,
// имя текущего тарифа, дни до истечения. tariffNames — кеш по ID для случаев,
// когда view строится для списка (чтобы не дёргать GetByID в цикле).
func (h *AdminHandler) buildUserView(
	ctx context.Context, u models.User, profs []adminProfileView, tariffNames map[int]string,
) adminUserView {
	// Для каждого профиля с личным лимитом считаем процент/цвет, чтобы
	// шаблон мог нарисовать прогресс-бар в таблице.
	for i := range profs {
		profs[i].TrafficTotal = profs[i].TrafficUp + profs[i].TrafficDown
		tl := profs[i].TrafficLimit
		if tl <= 0 {
			continue
		}
		pct := int(float64(profs[i].TrafficTotal) / float64(tl) * 100)
		if pct > 100 {
			pct = 100
		}
		profs[i].UsagePercent = pct
		profs[i].ProgressColor = progressColor(pct)
	}

	v := adminUserView{User: u, Profiles: profs}
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
		if tariffNames != nil {
			if name, ok := tariffNames[*u.CurrentTariffID]; ok {
				v.TariffLabel = name
			}
		}
		if v.TariffLabel == "" {
			if t, err := h.tariffs.GetByID(ctx, *u.CurrentTariffID); err == nil {
				v.TariffLabel = t.Label
			}
		}
	}
	if u.TariffExpiresAt != nil {
		d := time.Until(*u.TariffExpiresAt).Hours() / 24
		if d > 0 {
			v.DaysLeft = int(d) + 1
		}
	}

	// Base: процент использования подписочного лимита. Если лимита нет
	// (подписки нет / сброшен), остаётся 0 — шаблон не рисует бар.
	if u.BaseTrafficLimit > 0 {
		used := u.BaseTrafficUsed
		if used > u.BaseTrafficLimit {
			used = u.BaseTrafficLimit
		}
		v.BasePercent = int(float64(used) / float64(u.BaseTrafficLimit) * 100)
	}
	// Extra: процент потраченного от выданного в текущем цикле. granted
	// фиксируется при каждом пополнении, balance уменьшается при списании —
	// их разность и есть «потрачено из extra».
	v.ExtraUsed = u.ExtraTrafficGranted - u.ExtraTrafficBalance
	if v.ExtraUsed < 0 {
		v.ExtraUsed = 0
	}
	if u.ExtraTrafficGranted > 0 {
		used := v.ExtraUsed
		if used > u.ExtraTrafficGranted {
			used = u.ExtraTrafficGranted
		}
		v.ExtraPercent = int(float64(used) / float64(u.ExtraTrafficGranted) * 100)
	}
	return v
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

	// Имена тарифов для отображения текущей подписки юзера
	subPlans, err := h.tariffs.ListActiveByKind(r.Context(), models.TariffKindSubscription)
	if err != nil {
		log.Printf("Admin: list sub tariffs error: %v", err)
	}
	tariffNames := make(map[int]string)
	for _, t := range subPlans {
		tariffNames[t.ID] = t.Label
	}

	profilesByUser := make(map[int][]adminProfileView)
	for _, p := range profiles {
		profilesByUser[p.UserID] = append(profilesByUser[p.UserID], adminProfileView{
			VPNProfile: p,
			IsOnline:   p.IsActive && onlineUsers[p.UUID],
			OnlineIPs:  onlineIPs[p.UUID],
		})
	}

	views := make([]adminUserView, 0, len(users))
	for _, u := range users {
		views = append(views, h.buildUserView(r.Context(), u, profilesByUser[u.ID], tariffNames))
	}

	h.renderer.Render(w, "admin.html", map[string]any{
		"Active": "admin",
		"User":   middleware.UserFromContext(r.Context()),
		"Users":  views,
	})
}

// UserView — GET /admin/users/{id} — детальная карточка одного юзера со всеми
// формами управления. Быстрые действия остались на списке /admin, а детальные
// (подписка, extra, профили) доступны только отсюда.
func (h *AdminHandler) UserView(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	u, err := h.users.GetByID(r.Context(), id)
	if err != nil {
		http.Error(w, "Пользователь не найден", http.StatusNotFound)
		return
	}

	allProfs, err := h.profiles.GetByUserID(r.Context(), id)
	if err != nil {
		log.Printf("Admin: list profiles for user %d: %v", id, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// enrichAllProfiles умеет работать со срезом любого размера — переиспользуем.
	enriched, onlineUsers, onlineIPs := h.enrichAllProfiles(r.Context(), allProfs)
	profView := make([]adminProfileView, 0, len(enriched))
	for _, p := range enriched {
		profView = append(profView, adminProfileView{
			VPNProfile: p,
			IsOnline:   p.IsActive && onlineUsers[p.UUID],
			OnlineIPs:  onlineIPs[p.UUID],
		})
	}

	subPlans, err := h.tariffs.ListActiveByKind(r.Context(), models.TariffKindSubscription)
	if err != nil {
		log.Printf("Admin: list sub tariffs error: %v", err)
	}

	view := h.buildUserView(r.Context(), *u, profView, nil)

	// Если юзер пришёл по инвайту, покажем кликабельный бейдж с переходом на
	// страницу инвайта — для расследования утечки через учётку.
	var invite *models.Invite
	if u.InviteID != nil {
		if inv, err := h.invites.GetByID(r.Context(), *u.InviteID); err == nil {
			invite = inv
		}
	}

	h.renderer.Render(w, "user.html", map[string]any{
		"Active":   "admin",
		"User":     middleware.UserFromContext(r.Context()),
		"View":     view,
		"SubPlans": subPlans,
		"Invite":   invite,
		"ReturnTo": fmt.Sprintf("/admin/users/%d", u.ID),
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
			collector.RegisterProfile(profile.UUID, profile.ID, profile.UserID, profile.TrafficLimit)
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

	http.Redirect(w, r, adminRedirectBack(r), http.StatusSeeOther)
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

	http.Redirect(w, r, adminRedirectBack(r), http.StatusSeeOther)
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
			collector.RegisterProfile(profile.UUID, profile.ID, profile.UserID, profile.TrafficLimit)
		}
		log.Printf("Admin: profile %s reactivated after traffic reset", profile.UUID)
	}

	http.Redirect(w, r, adminRedirectBack(r), http.StatusSeeOther)
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
	http.Redirect(w, r, adminRedirectBack(r), http.StatusSeeOther)
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
	http.Redirect(w, r, adminRedirectBack(r), http.StatusSeeOther)
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

	http.Redirect(w, r, adminRedirectBack(r), http.StatusSeeOther)
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
	http.Redirect(w, r, adminRedirectBack(r), http.StatusSeeOther)
}

type adminProfileStatsJSON struct {
	ID              int      `json:"id"`
	IsActive        bool     `json:"is_active"`
	IsOnline        bool     `json:"is_online"`
	IsExpired       bool     `json:"is_expired"`
	IsOverLimit     bool     `json:"is_over_limit"`
	OnlineIPs       []string `json:"online_ips"`
	TrafficUp       string   `json:"traffic_up_fmt"`
	TrafficDown     string   `json:"traffic_down_fmt"`
	TrafficTotal    int64    `json:"traffic_total"`
	TrafficLimit    int64    `json:"traffic_limit"`
	TrafficTotalFmt string   `json:"traffic_total_fmt"`
	TrafficLimitFmt string   `json:"traffic_limit_fmt"`
	UsagePercent    int      `json:"usage_percent"`
	ProgressColor   string   `json:"progress_color"`
}

type adminUserStatsJSON struct {
	UserID      int    `json:"user_id"`
	OnlineCount int    `json:"online_count"`
	Total       string `json:"total_traffic_fmt"`
	BalanceFmt  string `json:"balance_fmt"`
	BaseFmt     string `json:"base_fmt"`  // "X / Y" для карточки юзера
	ExtraFmt    string `json:"extra_fmt"` // "X" для карточки юзера
	// Поля для прогресс-баров на /admin/users/{id} ("Баланс трафика").
	// В списке /admin не используются — JS просто не находит элементов.
	BaseUsed        int64                   `json:"base_used"`
	BaseLimit       int64                   `json:"base_limit"`
	BasePercent     int                     `json:"base_percent"`
	BaseUsedFmt     string                  `json:"base_used_fmt"`
	BaseLimitFmt    string                  `json:"base_limit_fmt"`
	ExtraUsed       int64                   `json:"extra_used"`
	ExtraGranted    int64                   `json:"extra_granted"`
	ExtraPercent    int                     `json:"extra_percent"`
	ExtraUsedFmt    string                  `json:"extra_used_fmt"`
	ExtraGrantedFmt string                  `json:"extra_granted_fmt"`
	Profiles        []adminProfileStatsJSON `json:"profiles"`
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
		basePct := 0
		if u.BaseTrafficLimit > 0 {
			used := u.BaseTrafficUsed
			if used > u.BaseTrafficLimit {
				used = u.BaseTrafficLimit
			}
			basePct = int(float64(used) / float64(u.BaseTrafficLimit) * 100)
		}
		extraUsed := u.ExtraTrafficGranted - u.ExtraTrafficBalance
		if extraUsed < 0 {
			extraUsed = 0
		}
		extraPct := 0
		if u.ExtraTrafficGranted > 0 {
			used := extraUsed
			if used > u.ExtraTrafficGranted {
				used = u.ExtraTrafficGranted
			}
			extraPct = int(float64(used) / float64(u.ExtraTrafficGranted) * 100)
		}
		uv := adminUserStatsJSON{
			UserID:          u.ID,
			BalanceFmt:      formatBytesGo(u.TotalAvailable()),
			BaseFmt:         formatBytesGo(u.BaseTrafficUsed) + " / " + formatBytesGo(u.BaseTrafficLimit),
			ExtraFmt:        formatBytesGo(u.ExtraTrafficBalance),
			BaseUsed:        u.BaseTrafficUsed,
			BaseLimit:       u.BaseTrafficLimit,
			BasePercent:     basePct,
			BaseUsedFmt:     formatBytesGo(u.BaseTrafficUsed),
			BaseLimitFmt:    formatBytesGo(u.BaseTrafficLimit),
			ExtraUsed:       extraUsed,
			ExtraGranted:    u.ExtraTrafficGranted,
			ExtraPercent:    extraPct,
			ExtraUsedFmt:    formatBytesGo(extraUsed),
			ExtraGrantedFmt: formatBytesGo(u.ExtraTrafficGranted),
		}
		var totalTraffic int64
		for _, p := range profilesByUser[u.ID] {
			totalTraffic += p.TrafficUp + p.TrafficDown
			if p.IsOnline {
				uv.OnlineCount++
			}
			profTotal := p.TrafficUp + p.TrafficDown
			profPct := 0
			profColor := ""
			if p.TrafficLimit > 0 {
				used := profTotal
				if used > p.TrafficLimit {
					used = p.TrafficLimit
				}
				profPct = int(float64(used) / float64(p.TrafficLimit) * 100)
				profColor = progressColor(profPct)
			}
			isExpired := p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now())
			isOverLimit := p.TrafficLimit > 0 && profTotal >= p.TrafficLimit
			uv.Profiles = append(uv.Profiles, adminProfileStatsJSON{
				ID:              p.ID,
				IsActive:        p.IsActive,
				IsOnline:        p.IsOnline,
				IsExpired:       isExpired,
				IsOverLimit:     isOverLimit,
				OnlineIPs:       p.OnlineIPs,
				TrafficUp:       formatBytesGo(p.TrafficUp),
				TrafficDown:     formatBytesGo(p.TrafficDown),
				TrafficTotal:    profTotal,
				TrafficLimit:    p.TrafficLimit,
				TrafficTotalFmt: formatBytesGo(profTotal),
				TrafficLimitFmt: formatBytesGo(p.TrafficLimit),
				UsagePercent:    profPct,
				ProgressColor:   profColor,
			})
		}
		uv.Total = formatBytesGo(totalTraffic)
		result = append(result, uv)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// UserTrafficChart — GET /admin/users/{id}/traffic?range=24h|7d|30d|90d.
// Агрегированные точки трафика конкретного юзера (сумма по всем его профилям).
// Используется админ-карточкой пользователя, поэтому без ownership-фильтра —
// admin middleware уже проверил права.
func (h *AdminHandler) UserTrafficChart(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	from, bucket, ok := resolveTrafficRange(r.URL.Query().Get("range"))
	if !ok {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}

	points, err := h.trafficLogs.AggregateByUser(r.Context(), userID, from, bucket)
	if err != nil {
		log.Printf("Admin UserTrafficChart: aggregate user=%d: %v", userID, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	if points == nil {
		points = []models.TrafficPoint{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"range":  r.URL.Query().Get("range"),
		"bucket": bucket,
		"points": points,
	})
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

// ──────────────────────────────────────────────
// Инвайты + режим регистрации
// ──────────────────────────────────────────────

// InvitesList — GET /admin/invites — список всех инвайтов + переключатель
// режима регистрации.
func (h *AdminHandler) InvitesList(w http.ResponseWriter, r *http.Request) {
	invites, err := h.invites.List(r.Context())
	if err != nil {
		log.Printf("Admin: list invites error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	mode, _ := h.invites.GetRegistrationMode(r.Context())

	h.renderer.Render(w, "invites.html", map[string]any{
		"Active":         "admin-invites",
		"User":           middleware.UserFromContext(r.Context()),
		"Invites":        invites,
		"Mode":           mode,
		"BaseURL":        h.baseURL,
		"ModeOpen":       models.RegModeOpen,
		"ModeInviteOnly": models.RegModeInviteOnly,
		"ModeBoth":       models.RegModeBoth,
		"ModeDisabled":   models.RegModeDisabled,
	})
}

// InviteCreate — POST /admin/invites — создать новый активный инвайт.
func (h *AdminHandler) InviteCreate(w http.ResponseWriter, r *http.Request) {
	note := strings.TrimSpace(r.FormValue("note"))
	if utf8.RuneCountInString(note) > 255 {
		http.Error(w, "Заметка слишком длинная (максимум 255 символов)", http.StatusBadRequest)
		return
	}
	var createdBy *int
	if u := middleware.UserFromContext(r.Context()); u != nil {
		createdBy = &u.ID
	}
	if _, err := h.invites.Create(r.Context(), note, createdBy); err != nil {
		log.Printf("Admin: invite create error: %v", err)
		http.Error(w, "Ошибка создания инвайта", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/invites", http.StatusSeeOther)
}

// InviteToggle — POST /admin/invites/{id}/toggle — вкл/выкл. Если инвайт
// уже soft-deleted, кнопка в UI спрятана, но на всякий случай позволяем:
// store не запрещает, а GetByCode всё равно такой инвайт не отдаст.
func (h *AdminHandler) InviteToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	action := r.FormValue("action")
	var active bool
	switch action {
	case "activate":
		active = true
	case "deactivate":
		active = false
	default:
		http.Error(w, "Неизвестное действие", http.StatusBadRequest)
		return
	}
	if err := h.invites.SetActive(r.Context(), id, active); err != nil {
		log.Printf("Admin: invite toggle error: %v", err)
		http.Error(w, "Ошибка", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/invites", http.StatusSeeOther)
}

// InviteDelete — POST /admin/invites/{id}/delete — soft-delete.
// Физически строку не удаляем, чтобы страница «кто заинвайтился» не ломалась.
func (h *AdminHandler) InviteDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	if err := h.invites.SoftDelete(r.Context(), id); err != nil {
		log.Printf("Admin: invite delete error: %v", err)
		http.Error(w, "Ошибка", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/invites", http.StatusSeeOther)
}

// InviteUsers — GET /admin/invites/{id}/users — список юзеров, пришедших
// по конкретному инвайту. Нужно для расследования утечки ссылки.
func (h *AdminHandler) InviteUsers(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	inv, err := h.invites.GetByID(r.Context(), id)
	if err != nil {
		http.Error(w, "Инвайт не найден", http.StatusNotFound)
		return
	}
	users, err := h.invites.ListUsersByInvite(r.Context(), id)
	if err != nil {
		log.Printf("Admin: list invite users error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.renderer.Render(w, "invite_users.html", map[string]any{
		"Active": "admin-invites",
		"User":   middleware.UserFromContext(r.Context()),
		"Invite": inv,
		"Users":  users,
	})
}

// SetRegistrationMode — POST /admin/settings/registration-mode
func (h *AdminHandler) SetRegistrationMode(w http.ResponseWriter, r *http.Request) {
	mode := strings.TrimSpace(r.FormValue("mode"))
	if !models.IsValidRegistrationMode(mode) {
		http.Error(w, "Недопустимый режим", http.StatusBadRequest)
		return
	}
	if err := h.invites.SetRegistrationMode(r.Context(), mode); err != nil {
		log.Printf("Admin: set registration mode error: %v", err)
		http.Error(w, "Ошибка", http.StatusInternalServerError)
		return
	}
	log.Printf("Admin: registration mode set to %q", mode)
	http.Redirect(w, r, "/admin/invites", http.StatusSeeOther)
}
