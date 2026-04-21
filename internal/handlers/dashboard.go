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
	xrayHolder *xray.Holder
	cfg        *config.Config
	renderer   *Renderer
}

func NewDashboardHandler(profiles *models.VPNProfileStore, users *models.UserStore, xrayHolder *xray.Holder, cfg *config.Config, renderer *Renderer) *DashboardHandler {
	return &DashboardHandler{profiles: profiles, users: users, xrayHolder: xrayHolder, cfg: cfg, renderer: renderer}
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
	Remaining     int64 // остаток лимита в байтах
}

// enrichProfiles добавляет live-трафик и онлайн-статус из snapshot или gRPC
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

func (h *DashboardHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
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

	// Пересчитываем баланс (user может быть stale из cookie)
	freshUser, _ := h.users.GetByID(r.Context(), user.ID)
	if freshUser != nil {
		user = freshUser
	}

	h.renderer.Render(w, "dashboard.html", map[string]any{
		"User":     user,
		"Profiles": views,
	})
}

func (h *DashboardHandler) CreateProfile(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	name := r.FormValue("name")
	if name == "" {
		name = "default"
	}

	// Парсим кол-во ГБ для профиля
	limitGBStr := r.FormValue("limit_gb")
	limitGB, err := strconv.ParseFloat(limitGBStr, 64)
	if err != nil || limitGB <= 0 {
		http.Error(w, "Укажите количество ГБ для профиля (> 0)", http.StatusBadRequest)
		return
	}

	limitBytes := int64(limitGB * 1024 * 1024 * 1024)

	// Списываем с баланса
	if err := h.users.DeductBalance(r.Context(), user.ID, limitBytes); err != nil {
		log.Printf("Deduct balance failed for user %d: %v", user.ID, err)
		http.Error(w, "Недостаточно трафика на балансе. Пополните баланс.", http.StatusBadRequest)
		return
	}

	newUUID := uuid.New().String()

	profile, err := h.profiles.Create(r.Context(), user.ID, newUUID, name)
	if err != nil {
		// Возвращаем баланс
		h.users.RefundBalance(r.Context(), user.ID, limitBytes)
		http.Error(w, "Ошибка создания профиля: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Устанавливаем лимит
	if err := h.profiles.SetLimit(r.Context(), profile.ID, limitBytes); err != nil {
		log.Printf("Warning: failed to set limit for profile %d: %v", profile.ID, err)
	}

	if client := h.xrayHolder.Get(); client != nil {
		if err := client.AddUser(r.Context(), newUUID, newUUID); err != nil {
			log.Printf("Warning: failed to add user to Xray: %v", err)
		}
	}

	// Обновляем лимит в collector
	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.UpdateLimit(newUUID, limitBytes)
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *DashboardHandler) DeleteProfile(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
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
		http.Error(w, "Профиль не найден", http.StatusNotFound)
		return
	}

	// Возвращаем неиспользованный трафик на баланс
	used := target.TrafficUp + target.TrafficDown
	if target.TrafficLimit > 0 && used < target.TrafficLimit {
		refund := target.TrafficLimit - used
		if err := h.users.RefundBalance(r.Context(), user.ID, refund); err != nil {
			log.Printf("Warning: failed to refund balance for user %d: %v", user.ID, err)
		} else {
			log.Printf("Refunded %d bytes to user %d (profile %s)", refund, user.ID, target.UUID)
		}
	}

	if client := h.xrayHolder.Get(); client != nil {
		if err := client.RemoveUser(r.Context(), target.UUID); err != nil {
			log.Printf("Warning: failed to remove user from Xray: %v", err)
		}
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
	Balance    int64              `json:"balance"`
	BalanceFmt string             `json:"balance_fmt"`
	Profiles   []profileStatsJSON `json:"profiles"`
}

func (h *DashboardHandler) StatsJSON(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	profiles, onlineUsers, onlineIPs := h.enrichProfiles(r.Context(), profiles)

	// Получаем актуальный баланс
	balance, _ := h.users.GetBalance(r.Context(), user.ID)

	profileStats := make([]profileStatsJSON, 0, len(profiles))
	for _, p := range profiles {
		total := p.TrafficUp + p.TrafficDown
		isExpired := p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now())
		isOverLimit := p.TrafficLimit > 0 && total >= p.TrafficLimit

		s := profileStatsJSON{
			ID:              p.ID,
			TrafficUp:       p.TrafficUp,
			TrafficDown:     p.TrafficDown,
			TrafficTotal:    total,
			TrafficUpFmt:    formatBytesGo(p.TrafficUp),
			TrafficDownFmt:  formatBytesGo(p.TrafficDown),
			TrafficTotalFmt: formatBytesGo(total),
			IsOnline:        p.IsActive && onlineUsers[p.UUID],
			OnlineIPs:       onlineIPs[p.UUID],
			IsActive:        p.IsActive,
			IsExpired:       isExpired,
			IsOverLimit:     isOverLimit,
		}

		if p.TrafficLimit > 0 {
			pct := int(float64(total) / float64(p.TrafficLimit) * 100)
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

	result := dashStatsJSON{
		Balance:    balance,
		BalanceFmt: formatBytesGo(balance),
		Profiles:   profileStats,
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
