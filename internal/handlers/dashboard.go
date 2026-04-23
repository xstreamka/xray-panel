package handlers

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xray-panel/internal/config"
	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
	"xray-panel/internal/xray"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type DashboardHandler struct {
	profiles   *models.VPNProfileStore
	users      *models.UserStore
	tariffs    *models.TariffStore
	xrayHolder *xray.Holder
	cfg        *config.Config
	renderer   *Renderer
}

func NewDashboardHandler(
	profiles *models.VPNProfileStore,
	users *models.UserStore,
	tariffs *models.TariffStore,
	xrayHolder *xray.Holder,
	cfg *config.Config,
	renderer *Renderer,
) *DashboardHandler {
	return &DashboardHandler{
		profiles: profiles, users: users, tariffs: tariffs,
		xrayHolder: xrayHolder, cfg: cfg, renderer: renderer,
	}
}

type profileView struct {
	models.VPNProfile
	VlessURI      template.URL
	TrafficTotal  int64
	UsagePercent  int
	ProgressColor string
	IsExpired     bool
	IsOverLimit   bool
	IsOnline      bool
	OnlineIPs     []string
	Remaining     int64 // остаток ЛОКАЛЬНОГО лимита профиля (если задан)
}

// errorAction — дополнительная кнопка на странице ошибки помимо «Назад».
type errorAction struct {
	Label string
	URL   string
}

// renderError рендерит полноценную HTML-страницу с ошибкой и кнопкой «Назад»
// вместо голого http.Error — чтобы юзер не видел пустую страницу с plain text
// после submit формы.
func (h *DashboardHandler) renderError(w http.ResponseWriter, r *http.Request, status int, title, message string, actions ...errorAction) {
	backURL := r.Referer()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = h.renderer.Render(w, "error.html", map[string]any{
		"Title":   title,
		"Message": message,
		"BackURL": backURL,
		"Actions": actions,
	})
}

// assertBalance — централизованный gate для пользовательских write-действий
// над профилями. Если у юзера нет доступного трафика, любые операции, способные
// (явно или косвенно) реактивировать профиль либо запустить новый, должны быть
// отклонены — иначе профиль на пару секунд оживёт и тут же будет вырублен
// коллектором (плохой UX и пустая нагрузка). Возвращает свежего юзера из БД,
// чтобы вызывающий код использовал его для дальнейшей логики.
func (h *DashboardHandler) assertBalance(w http.ResponseWriter, r *http.Request) (*models.User, bool) {
	ctxUser := middleware.UserFromContext(r.Context())
	user, err := h.users.GetByID(r.Context(), ctxUser.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Ошибка", "Внутренняя ошибка сервера. Попробуйте позже.")
		return nil, false
	}
	if user.TotalAvailable() <= 0 {
		h.renderError(w, r,
			http.StatusForbidden,
			"Нет доступного трафика",
			"Чтобы управлять VPN профилями, оформите или продлите тариф.",
			errorAction{Label: "Оплатить тариф", URL: "/pay"},
		)
		return nil, false
	}
	return user, true
}

func (h *DashboardHandler) enrichProfiles(ctx context.Context, profiles []models.VPNProfile) (
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

// Welcome — главная страница после авторизации.
// Показывает приветствие, статус подписки, описание сервиса и актуальные тарифы.
func (h *DashboardHandler) Welcome(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	freshUser, _ := h.users.GetByID(r.Context(), user.ID)
	if freshUser != nil {
		user = freshUser
	}

	var tariffLabel string
	if user.CurrentTariffID != nil {
		if t, err := h.tariffs.GetByID(r.Context(), *user.CurrentTariffID); err == nil {
			tariffLabel = t.Label
		}
	}

	subPlans, err := h.tariffs.ListActiveByKind(r.Context(), models.TariffKindSubscription)
	if err != nil {
		log.Printf("Welcome: list sub tariffs error: %v", err)
	}
	addonPlans, err := h.tariffs.ListActiveByKind(r.Context(), models.TariffKindAddon)
	if err != nil {
		log.Printf("Welcome: list addon tariffs error: %v", err)
	}

	h.renderer.Render(w, "welcome.html", map[string]any{
		"Active":              "welcome",
		"User":                user,
		"TariffLabel":         tariffLabel,
		"HasActiveSub":        user.HasActiveSubscription(),
		"SubscriptionTariffs": subPlans,
		"AddonTariffs":        addonPlans,
	})
}

func (h *DashboardHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Ошибка", "Не удалось загрузить список профилей. Попробуйте позже.")
		return
	}

	profiles, onlineUsers, onlineIPs := h.enrichProfiles(r.Context(), profiles)

	var views []profileView
	for _, p := range profiles {
		v := profileView{
			VPNProfile:   p,
			VlessURI:     template.URL(h.buildVlessURI(p.UUID, p.Name)),
			TrafficTotal: p.TrafficUp + p.TrafficDown,
			IsOnline:     p.IsActive && onlineUsers[p.UUID],
			OnlineIPs:    onlineIPs[p.UUID],
		}

		// Лимит на профиле — опциональный "родительский контроль".
		// 0 = без лимита, enforce работает только по балансу юзера.
		if p.TrafficLimit > 0 {
			used := v.TrafficTotal
			v.Remaining = p.TrafficLimit - used
			if v.Remaining < 0 {
				v.Remaining = 0
			}
			pct := int(float64(used) / float64(p.TrafficLimit) * 100)
			if pct > 100 {
				pct = 100
			}
			v.UsagePercent = pct
			v.IsOverLimit = used >= p.TrafficLimit
			switch {
			case pct >= 90:
				v.ProgressColor = "#ef4444"
			case pct >= 70:
				v.ProgressColor = "#f59e0b"
			default:
				v.ProgressColor = "#22c55e"
			}
		}

		if p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now()) {
			v.IsExpired = true
		}

		views = append(views, v)
	}

	// Свежий юзер для состояния подписки и баланса (cookie может быть stale)
	freshUser, _ := h.users.GetByID(r.Context(), user.ID)
	if freshUser != nil {
		user = freshUser
	}

	// Имя текущего тарифа (если есть)
	var tariffLabel string
	if user.CurrentTariffID != nil {
		if t, err := h.tariffs.GetByID(r.Context(), *user.CurrentTariffID); err == nil {
			tariffLabel = t.Label
		}
	}

	baseRemaining := user.BaseTrafficLimit - user.BaseTrafficUsed
	if baseRemaining < 0 {
		baseRemaining = 0
	}

	var basePercent int
	if user.BaseTrafficLimit > 0 {
		used := user.BaseTrafficUsed
		if used > user.BaseTrafficLimit {
			used = user.BaseTrafficLimit
		}
		basePercent = int(float64(used) / float64(user.BaseTrafficLimit) * 100)
	}

	// extra: сколько потрачено из выданного за текущий цикл.
	// granted фиксируется при каждом пополнении, balance уменьшается при списании.
	extraUsed := user.ExtraTrafficGranted - user.ExtraTrafficBalance
	if extraUsed < 0 {
		extraUsed = 0
	}
	var extraPercent int
	if user.ExtraTrafficGranted > 0 {
		used := extraUsed
		if used > user.ExtraTrafficGranted {
			used = user.ExtraTrafficGranted
		}
		extraPercent = int(float64(used) / float64(user.ExtraTrafficGranted) * 100)
	}

	var daysLeft int
	if user.TariffExpiresAt != nil {
		d := time.Until(*user.TariffExpiresAt).Hours() / 24
		if d > 0 {
			daysLeft = int(d) + 1
		}
	}

	h.renderer.Render(w, "dashboard.html", map[string]any{
		"Active":         "dashboard",
		"User":           user,
		"Profiles":       views,
		"TariffLabel":    tariffLabel,
		"BaseRemaining":  baseRemaining,
		"BasePercent":    basePercent,
		"ExtraUsed":      extraUsed,
		"ExtraPercent":   extraPercent,
		"DaysLeft":       daysLeft,
		"HasActiveSub":   user.HasActiveSubscription(),
		"TotalAvailable": user.TotalAvailable(),
	})
}

func (h *DashboardHandler) CreateProfile(w http.ResponseWriter, r *http.Request) {
	user, ok := h.assertBalance(w, r)
	if !ok {
		return
	}
	name := r.FormValue("name")
	if name == "" {
		name = "default"
	}

	// limit_gb теперь опционален: это ЛОКАЛЬНЫЙ лимит устройства (parental).
	// Пусто или 0 = без лимита, трафик просто списывается из общего пула юзера.
	var limitBytes int64
	if s := strings.TrimSpace(r.FormValue("limit_gb")); s != "" {
		limitGB, err := strconv.ParseFloat(s, 64)
		if err != nil || limitGB < 0 {
			h.renderError(w, r, http.StatusBadRequest, "Ошибка", "Некорректное значение лимита.")
			return
		}
		limitBytes = int64(limitGB * 1024 * 1024 * 1024)
	}

	newUUID := uuid.New().String()

	profile, err := h.profiles.Create(r.Context(), user.ID, newUUID, name)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Не удалось создать профиль", err.Error())
		return
	}

	if limitBytes > 0 {
		if err := h.profiles.SetLimit(r.Context(), profile.ID, limitBytes); err != nil {
			log.Printf("Warning: failed to set limit for profile %d: %v", profile.ID, err)
		}
	}

	if client := h.xrayHolder.Get(); client != nil {
		if err := client.AddUser(r.Context(), newUUID, newUUID); err != nil {
			log.Printf("Warning: failed to add user to Xray: %v", err)
		}
	}

	// Регистрируем профиль в кэшах коллектора: uuid→user_id + personal-лимит.
	// Без этого списание трафика с user-баланса для свежего профиля не работало бы
	// до ближайшей ресинхронизации (раз в минуту).
	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.RegisterProfile(newUUID, user.ID, limitBytes)
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// SetProfileLimit — POST /dashboard/profiles/{id}/limit — юзер сам задаёт
// или меняет personal-лимит своего профиля (0 = безлимит, трафик списывается
// только из общего подписочного пула).
func (h *DashboardHandler) SetProfileLimit(w http.ResponseWriter, r *http.Request) {
	user, ok := h.assertBalance(w, r)
	if !ok {
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Ошибка", "Некорректный идентификатор профиля.")
		return
	}

	limitGB, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("limit_gb")), 64)
	if err != nil || limitGB < 0 {
		h.renderError(w, r, http.StatusBadRequest, "Ошибка", "Некорректное значение лимита.")
		return
	}
	limitBytes := int64(limitGB * 1024 * 1024 * 1024)

	// Проверка ownership: юзер может менять только свои профили.
	profile, err := h.profiles.GetByID(r.Context(), id)
	if err != nil || profile.UserID != user.ID {
		h.renderError(w, r, http.StatusNotFound, "Профиль не найден", "Проверьте, что вы редактируете свой профиль.")
		return
	}

	if err := h.profiles.SetLimit(r.Context(), id, limitBytes); err != nil {
		log.Printf("Dashboard: set limit error profile=%d user=%d: %v", id, user.ID, err)
		h.renderError(w, r, http.StatusInternalServerError, "Ошибка", "Не удалось сохранить лимит. Попробуйте позже.")
		return
	}

	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.UpdateLimit(profile.UUID, limitBytes)
	}

	// Если профиль был отключён из-за превышения personal-лимита, и новый
	// лимит больше уже накопленного трафика (или 0 = безлимит) — реактивируем.
	// Коллектор сам проверит user-баланс и отключит снова, если тот исчерпан.
	if !profile.IsActive {
		totalTraffic := profile.TrafficUp + profile.TrafficDown
		canActivate := limitBytes == 0 || totalTraffic < limitBytes
		if canActivate {
			h.reactivateProfile(r.Context(), profile, limitBytes)
		}
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// ToggleProfile — POST /dashboard/profiles/{id}/toggle — юзер сам включает
// или выключает свой профиль. Выключенный профиль остаётся в БД с is_active=false;
// URI для него не принимается Xray (юзер удалён из inbound). Статистика
// трафика сохраняется — после включения будет продолжаться с того же числа.
//
// Форма передаёт action=activate или action=deactivate.
// При активации проверяется personal-лимит и общий баланс юзера: иначе
// профиль включился бы на пару секунд и тут же выключился коллектором.
func (h *DashboardHandler) ToggleProfile(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Ошибка", "Некорректный идентификатор профиля.")
		return
	}

	profile, err := h.profiles.GetByID(r.Context(), id)
	if err != nil || profile.UserID != user.ID {
		h.renderError(w, r, http.StatusNotFound, "Профиль не найден", "Проверьте, что вы управляете своим профилем.")
		return
	}

	switch r.FormValue("action") {
	case "activate":
		if profile.IsActive {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
		if _, ok := h.assertBalance(w, r); !ok {
			return
		}
		if profile.TrafficLimit > 0 && profile.TrafficUp+profile.TrafficDown >= profile.TrafficLimit {
			h.renderError(w, r,
				http.StatusBadRequest,
				"Лимит устройства превышен",
				"Увеличьте лимит или поставьте 0 (безлимит) и повторите активацию.",
			)
			return
		}
		h.reactivateProfile(r.Context(), profile, profile.TrafficLimit)

	case "deactivate":
		if !profile.IsActive {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
		h.deactivateProfile(r.Context(), profile)

	default:
		h.renderError(w, r, http.StatusBadRequest, "Ошибка", "Неизвестное действие.")
		return
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// reactivateProfile возвращает один профиль в работу: is_active=true в БД,
// AddUser в Xray, обновление personal-лимита в кэше коллектора.
// Не отвечает за проверку условий — это делает вызывающий код.
func (h *DashboardHandler) reactivateProfile(ctx context.Context, p *models.VPNProfile, limitBytes int64) {
	if err := h.profiles.SetActive(ctx, p.ID, true); err != nil {
		log.Printf("Dashboard: reactivate SetActive profile=%d: %v", p.ID, err)
		return
	}
	if client := h.xrayHolder.Get(); client != nil {
		if err := client.AddUser(ctx, p.UUID, p.UUID); err != nil {
			log.Printf("Dashboard: reactivate AddUser profile=%d: %v", p.ID, err)
		}
	}
	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.RegisterProfile(p.UUID, p.UserID, limitBytes)
	}
	log.Printf("Dashboard: profile %s reactivated by user", p.UUID)
}

// deactivateProfile снимает is_active в БД, убивает активные соединения
// через ss-K и удаляет UUID из Xray, чтобы никто не мог переподключиться.
// Статистика трафика на профиле сохраняется.
func (h *DashboardHandler) deactivateProfile(ctx context.Context, p *models.VPNProfile) {
	if err := h.profiles.SetActive(ctx, p.ID, false); err != nil {
		log.Printf("Dashboard: deactivate SetActive profile=%d: %v", p.ID, err)
		return
	}
	if client := h.xrayHolder.Get(); client != nil {
		if fw := h.xrayHolder.GetFirewall(); fw != nil {
			fw.BlockUser(ctx, client, p.UUID)
		}
		if err := client.RemoveUser(ctx, p.UUID); err != nil {
			log.Printf("Dashboard: deactivate RemoveUser profile=%d: %v", p.ID, err)
		}
	}
	log.Printf("Dashboard: profile %s deactivated by user", p.UUID)
}

// ResetProfileTraffic — POST /dashboard/profiles/{id}/reset — обнуляет
// trafic_up / traffic_down профиля. Юзерская фича «чистый старт» для
// устройств, у которых исчерпан personal-лимит. Общий подписочный баланс
// юзера не трогает — он считается централизованно в user-модели.
// Если профиль был отключён по personal-лимиту — реактивируем.
func (h *DashboardHandler) ResetProfileTraffic(w http.ResponseWriter, r *http.Request) {
	user, ok := h.assertBalance(w, r)
	if !ok {
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Ошибка", "Некорректный идентификатор профиля.")
		return
	}

	profile, err := h.profiles.GetByID(r.Context(), id)
	if err != nil || profile.UserID != user.ID {
		h.renderError(w, r, http.StatusNotFound, "Профиль не найден", "Проверьте, что вы сбрасываете трафик на своём профиле.")
		return
	}

	if err := h.profiles.ResetTraffic(r.Context(), id); err != nil {
		log.Printf("Dashboard: reset traffic profile=%d: %v", id, err)
		h.renderError(w, r, http.StatusInternalServerError, "Ошибка", "Не удалось сбросить трафик. Попробуйте позже.")
		return
	}

	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.ResetCumulative(profile.UUID)
	}

	if !profile.IsActive {
		h.reactivateProfile(r.Context(), profile, profile.TrafficLimit)
	}

	log.Printf("Dashboard: profile %s traffic reset by user", profile.UUID)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *DashboardHandler) DeleteProfile(w http.ResponseWriter, r *http.Request) {
	user, ok := h.assertBalance(w, r)
	if !ok {
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Ошибка", "Некорректный идентификатор профиля.")
		return
	}

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Ошибка", "Внутренняя ошибка сервера. Попробуйте позже.")
		return
	}

	var target *models.VPNProfile
	for _, p := range profiles {
		if p.ID == id {
			target = &p
			break
		}
	}
	if target == nil {
		h.renderError(w, r, http.StatusNotFound, "Профиль не найден", "Проверьте, что вы удаляете свой профиль.")
		return
	}

	// В новой модели трафик живёт на юзере, возвращать при удалении нечего.
	// Просто удаляем профиль из Xray и БД.
	if client := h.xrayHolder.Get(); client != nil {
		if err := client.RemoveUser(r.Context(), target.UUID); err != nil {
			log.Printf("Warning: failed to remove user from Xray: %v", err)
		}
	}

	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.UnregisterProfile(target.UUID)
	}

	if _, err := h.profiles.Delete(r.Context(), id); err != nil {
		log.Printf("Delete profile error: %v", err)
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

type profileStatsJSON struct {
	ID              int      `json:"id"`
	TrafficUp       int64    `json:"traffic_up"`
	TrafficDown     int64    `json:"traffic_down"`
	TrafficTotal    int64    `json:"traffic_total"`
	TrafficUpFmt    string   `json:"traffic_up_fmt"`
	TrafficDownFmt  string   `json:"traffic_down_fmt"`
	TrafficTotalFmt string   `json:"traffic_total_fmt"`
	UsagePercent    int      `json:"usage_percent"`
	ProgressColor   string   `json:"progress_color"`
	LimitFmt        string   `json:"limit_fmt,omitzero"`
	IsOnline        bool     `json:"is_online"`
	OnlineIPs       []string `json:"online_ips"`
	IsActive        bool     `json:"is_active"`
	IsExpired       bool     `json:"is_expired"`
	IsOverLimit     bool     `json:"is_over_limit"`
}

type dashStatsJSON struct {
	// Общий доступный трафик: base_room + extra (то, что показываем как "баланс")
	Balance    int64  `json:"balance"`
	BalanceFmt string `json:"balance_fmt"`
	// Детализация для полноценного UI
	BaseLimit        int64  `json:"base_limit"`
	BaseLimitFmt     string `json:"base_limit_fmt"`
	BaseUsed         int64  `json:"base_used"`
	BaseUsedFmt      string `json:"base_used_fmt"`
	BaseRemaining    int64  `json:"base_remaining"`
	BaseRemainingFmt string `json:"base_remaining_fmt"`
	ExtraBalance     int64  `json:"extra_balance"`
	ExtraBalanceFmt  string `json:"extra_balance_fmt"`
	ExtraGranted     int64  `json:"extra_granted"`
	ExtraGrantedFmt  string `json:"extra_granted_fmt"`
	ExtraUsed        int64  `json:"extra_used"`
	ExtraUsedFmt     string `json:"extra_used_fmt"`
	FrozenBalance    int64  `json:"frozen_balance"`
	FrozenBalanceFmt string `json:"frozen_balance_fmt"`
	// Подписка
	TariffLabel     string     `json:"tariff_label"`
	TariffExpiresAt *time.Time `json:"tariff_expires_at"`
	DaysLeft        int        `json:"days_left"`
	HasActiveSub    bool       `json:"has_active_subscription"`

	Profiles []profileStatsJSON `json:"profiles"`
}

func (h *DashboardHandler) StatsJSON(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	profiles, onlineUsers, onlineIPs := h.enrichProfiles(r.Context(), profiles)

	// Свежий юзер для актуальных балансов и подписки
	freshUser, _ := h.users.GetByID(r.Context(), user.ID)
	if freshUser != nil {
		user = freshUser
	}

	baseRem := user.BaseTrafficLimit - user.BaseTrafficUsed
	if baseRem < 0 {
		baseRem = 0
	}
	total := baseRem + user.ExtraTrafficBalance

	profileStats := make([]profileStatsJSON, 0, len(profiles))
	for _, p := range profiles {
		totalP := p.TrafficUp + p.TrafficDown
		isExpired := p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now())
		isOverLimit := p.TrafficLimit > 0 && totalP >= p.TrafficLimit

		s := profileStatsJSON{
			ID:              p.ID,
			TrafficUp:       p.TrafficUp,
			TrafficDown:     p.TrafficDown,
			TrafficTotal:    totalP,
			TrafficUpFmt:    formatBytesGo(p.TrafficUp),
			TrafficDownFmt:  formatBytesGo(p.TrafficDown),
			TrafficTotalFmt: formatBytesGo(totalP),
			IsOnline:        p.IsActive && onlineUsers[p.UUID],
			OnlineIPs:       onlineIPs[p.UUID],
			IsActive:        p.IsActive,
			IsExpired:       isExpired,
			IsOverLimit:     isOverLimit,
		}

		if p.TrafficLimit > 0 {
			pct := int(float64(totalP) / float64(p.TrafficLimit) * 100)
			if pct > 100 {
				pct = 100
			}
			s.UsagePercent = pct
			s.LimitFmt = formatBytesGo(p.TrafficLimit)
			switch {
			case pct >= 90:
				s.ProgressColor = "#ef4444"
			case pct >= 70:
				s.ProgressColor = "#f59e0b"
			default:
				s.ProgressColor = "#22c55e"
			}
		}

		profileStats = append(profileStats, s)
	}

	var tariffLabel string
	if user.CurrentTariffID != nil {
		if t, err := h.tariffs.GetByID(r.Context(), *user.CurrentTariffID); err == nil {
			tariffLabel = t.Label
		}
	}

	var daysLeft int
	if user.TariffExpiresAt != nil {
		d := time.Until(*user.TariffExpiresAt).Hours() / 24
		if d > 0 {
			daysLeft = int(d) + 1 // округляем вверх: "осталось 3 дня", даже если это 2.1
		}
	}

	result := dashStatsJSON{
		Balance:          total,
		BalanceFmt:       formatBytesGo(total),
		BaseLimit:        user.BaseTrafficLimit,
		BaseLimitFmt:     formatBytesGo(user.BaseTrafficLimit),
		BaseUsed:         user.BaseTrafficUsed,
		BaseUsedFmt:      formatBytesGo(user.BaseTrafficUsed),
		BaseRemaining:    baseRem,
		BaseRemainingFmt: formatBytesGo(baseRem),
		ExtraBalance:     user.ExtraTrafficBalance,
		ExtraBalanceFmt:  formatBytesGo(user.ExtraTrafficBalance),
		ExtraGranted:     user.ExtraTrafficGranted,
		ExtraGrantedFmt:  formatBytesGo(user.ExtraTrafficGranted),
		ExtraUsed:        max(0, user.ExtraTrafficGranted-user.ExtraTrafficBalance),
		ExtraUsedFmt:     formatBytesGo(max(0, user.ExtraTrafficGranted-user.ExtraTrafficBalance)),
		FrozenBalance:    user.FrozenExtraBalance,
		FrozenBalanceFmt: formatBytesGo(user.FrozenExtraBalance),
		TariffLabel:      tariffLabel,
		TariffExpiresAt:  user.TariffExpiresAt,
		DaysLeft:         daysLeft,
		HasActiveSub:     user.HasActiveSubscription(),
		Profiles:         profileStats,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func formatBytesGo(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), []string{"KB", "MB", "GB", "TB"}[exp])
}

func (h *DashboardHandler) buildVlessURI(userUUID, name string) string {
	return fmt.Sprintf(
		"vless://%s@%s:%s?type=tcp&security=reality&sni=%s&fp=chrome&pbk=%s&sid=%s&flow=xtls-rprx-vision#%s",
		userUUID,
		h.cfg.ServerAddr,
		h.cfg.ServerPort,
		cmp.Or(h.cfg.RealityServerName, "9.9.9.9"),
		h.cfg.RealityPublicKey,
		h.cfg.RealityShortID,
		name,
	)
}
