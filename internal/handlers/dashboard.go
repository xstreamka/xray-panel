package handlers

import (
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
	xrayHolder *xray.Holder
	cfg        *config.Config
	renderer   *Renderer
}

func NewDashboardHandler(profiles *models.VPNProfileStore, xrayHolder *xray.Holder, cfg *config.Config, renderer *Renderer) *DashboardHandler {
	return &DashboardHandler{profiles: profiles, xrayHolder: xrayHolder, cfg: cfg, renderer: renderer}
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
	DeviceCount   int
}

func (h *DashboardHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Подтягиваем свежую статистику, онлайн-статус и количество устройств из Xray
	onlineUsers := make(map[string]bool)
	onlineIPCounts := make(map[string]int)
	if client := h.xrayHolder.Get(); client != nil {
		if liveTraffic, err := client.QueryAllUserTraffic(r.Context(), false); err == nil {
			for i, p := range profiles {
				if stats, ok := liveTraffic[p.UUID]; ok {
					profiles[i].TrafficUp += stats[0]
					profiles[i].TrafficDown += stats[1]
				}
			}
		}
		if online, err := client.GetOnlineUsers(r.Context()); err == nil {
			onlineUsers = online
		}
		if counts, err := client.GetOnlineIPCounts(r.Context()); err == nil {
			onlineIPCounts = counts
		}
	}

	var views []profileView
	for _, p := range profiles {
		v := profileView{
			VPNProfile:   p,
			VlessURI:     template.URL(h.buildVlessURI(p.UUID, p.Name)),
			TrafficTotal: p.TrafficUp + p.TrafficDown,
			IsOnline:     onlineUsers[p.UUID],
			DeviceCount:  onlineIPCounts[p.UUID],
		}

		if p.TrafficLimit > 0 {
			pct := int(float64(v.TrafficTotal) / float64(p.TrafficLimit) * 100)
			if pct > 100 {
				pct = 100
			}
			v.UsagePercent = pct
			v.IsOverLimit = v.TrafficTotal >= p.TrafficLimit

			switch {
			case pct >= 90:
				v.ProgressColor = "#ef4444" // красный
			case pct >= 70:
				v.ProgressColor = "#f59e0b" // жёлтый
			default:
				v.ProgressColor = "#22c55e" // зелёный
			}
		}

		if p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now()) {
			v.IsExpired = true
		}

		views = append(views, v)
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

	newUUID := uuid.New().String()

	_, err := h.profiles.Create(r.Context(), user.ID, newUUID, name)
	if err != nil {
		http.Error(w, "Ошибка создания профиля: "+err.Error(), http.StatusBadRequest)
		return
	}

	if client := h.xrayHolder.Get(); client != nil {
		if err := client.AddUser(r.Context(), newUUID, newUUID); err != nil {
			log.Printf("Warning: failed to add user to Xray: %v", err)
		}
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

	// Проверяем что профиль принадлежит юзеру
	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	var targetUUID string
	for _, p := range profiles {
		if p.ID == id {
			targetUUID = p.UUID
			break
		}
	}

	if targetUUID == "" {
		http.Error(w, "Профиль не найден", http.StatusNotFound)
		return
	}

	// Удаляем из Xray
	if client := h.xrayHolder.Get(); client != nil {
		if err := client.RemoveUser(r.Context(), targetUUID); err != nil {
			log.Printf("Warning: failed to remove user from Xray: %v", err)
		}
	}

	// Удаляем из БД
	if _, err := h.profiles.Delete(r.Context(), id); err != nil {
		log.Printf("Delete profile error: %v", err)
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

type profileStatsJSON struct {
	ID              int    `json:"id"`
	TrafficUp       int64  `json:"traffic_up"`
	TrafficDown     int64  `json:"traffic_down"`
	TrafficTotal    int64  `json:"traffic_total"`
	TrafficUpFmt    string `json:"traffic_up_fmt"`
	TrafficDownFmt  string `json:"traffic_down_fmt"`
	TrafficTotalFmt string `json:"traffic_total_fmt"`
	UsagePercent    int    `json:"usage_percent"`
	ProgressColor   string `json:"progress_color"`
	LimitFmt        string `json:"limit_fmt,omitzero"`
}

func (h *DashboardHandler) StatsJSON(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	if client := h.xrayHolder.Get(); client != nil {
		if liveTraffic, err := client.QueryAllUserTraffic(r.Context(), false); err == nil {
			for i, p := range profiles {
				if stats, ok := liveTraffic[p.UUID]; ok {
					profiles[i].TrafficUp += stats[0]
					profiles[i].TrafficDown += stats[1]
				}
			}
		}
	}

	result := make([]profileStatsJSON, 0, len(profiles))
	for _, p := range profiles {
		total := p.TrafficUp + p.TrafficDown
		s := profileStatsJSON{
			ID:              p.ID,
			TrafficUp:       p.TrafficUp,
			TrafficDown:     p.TrafficDown,
			TrafficTotal:    total,
			TrafficUpFmt:    formatBytesGo(p.TrafficUp),
			TrafficDownFmt:  formatBytesGo(p.TrafficDown),
			TrafficTotalFmt: formatBytesGo(total),
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

		result = append(result, s)
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
		h.cfg.RealityServerName,
		h.cfg.RealityPublicKey,
		h.cfg.RealityShortID,
		name,
	)
}
