package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"xray-panel/internal/models"
	"xray-panel/internal/xray"

	"github.com/go-chi/chi/v5"
)

type AdminHandler struct {
	users      *models.UserStore
	profiles   *models.VPNProfileStore
	xrayHolder *xray.Holder
	renderer   *Renderer
}

func NewAdminHandler(users *models.UserStore, profiles *models.VPNProfileStore, xrayHolder *xray.Holder, renderer *Renderer) *AdminHandler {
	return &AdminHandler{users: users, profiles: profiles, xrayHolder: xrayHolder, renderer: renderer}
}

// enrichAllProfiles добавляет live-трафик и онлайн-статус ко всем профилям
func (h *AdminHandler) enrichAllProfiles(ctx context.Context, profiles []models.VPNProfile) (
	enriched []models.VPNProfile,
	onlineUsers map[string]bool,
	onlineIPs map[string][]string,
) {
	enriched = profiles
	onlineUsers = make(map[string]bool)

	// Сначала пробуем snapshot
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

	// Fallback: прямой gRPC
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
		views = append(views, v)
	}

	h.renderer.Render(w, "admin.html", map[string]any{
		"Users": views,
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
		// Убираем iptables-блокировку
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
		// Обновляем лимит в collector
		if collector := h.xrayHolder.GetCollector(); collector != nil {
			collector.UpdateLimit(profile.UUID, profile.TrafficLimit)
		}
		log.Printf("Admin: profile %s activated", profile.UUID)

	case "deactivate":
		if err := h.profiles.SetActive(r.Context(), id, false); err != nil {
			log.Printf("Admin: deactivate error: %v", err)
		}
		if client := h.xrayHolder.Get(); client != nil {
			// Блокируем TCP через iptables (правила живут до реактивации)
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

	// Обновляем лимит в кэше collector'а
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

	// Сбрасываем кумулятивный счётчик в collector'е
	if collector := h.xrayHolder.GetCollector(); collector != nil {
		collector.ResetCumulative(profile.UUID)
	}

	if !profile.IsActive {
		// Убираем iptables-блокировку
		if fw := h.xrayHolder.GetFirewall(); fw != nil {
			fw.UnblockUser(profile.UUID)
		}
		h.profiles.SetActive(r.Context(), id, true)
		if client := h.xrayHolder.Get(); client != nil {
			client.AddUser(r.Context(), profile.UUID, profile.UUID)
		}
		// Восстанавливаем лимит в collector
		if collector := h.xrayHolder.GetCollector(); collector != nil {
			collector.UpdateLimit(profile.UUID, profile.TrafficLimit)
		}
		log.Printf("Admin: profile %s reactivated after traffic reset", profile.UUID)
	}

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
		uv := adminUserStatsJSON{UserID: u.ID}
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
