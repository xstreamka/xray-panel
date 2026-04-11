package handlers

import (
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

	// Группируем профили по user_id
	profilesByUser := make(map[int][]models.VPNProfile)
	for _, p := range profiles {
		profilesByUser[p.UserID] = append(profilesByUser[p.UserID], p)
	}

	type userView struct {
		models.User
		Profiles     []models.VPNProfile
		TotalTraffic int64
	}

	var views []userView
	for _, u := range users {
		v := userView{User: u, Profiles: profilesByUser[u.ID]}
		for _, p := range v.Profiles {
			v.TotalTraffic += p.TrafficUp + p.TrafficDown
		}
		views = append(views, v)
	}

	h.renderer.Render(w, "admin.html", map[string]any{
		"Users": views,
	})
}

func (h *AdminHandler) ToggleProfile(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	action := r.FormValue("action") // "activate" или "deactivate"

	profile, err := h.profiles.GetByID(r.Context(), id)
	if err != nil {
		http.Error(w, "Профиль не найден", http.StatusNotFound)
		return
	}

	switch action {
	case "activate":
		if err := h.profiles.SetActive(r.Context(), id, true); err != nil {
			log.Printf("Admin: activate error: %v", err)
		}
		if client := h.xrayHolder.Get(); client != nil {
			if err := client.AddUser(r.Context(), profile.UUID, profile.UUID); err != nil {
				log.Printf("Admin: xray add error: %v", err)
			}
		}
		log.Printf("Admin: profile %s activated", profile.UUID)

	case "deactivate":
		if err := h.profiles.SetActive(r.Context(), id, false); err != nil {
			log.Printf("Admin: deactivate error: %v", err)
		}
		if client := h.xrayHolder.Get(); client != nil {
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

	// Если профиль был деактивирован из-за лимита — включаем обратно
	if !profile.IsActive {
		h.profiles.SetActive(r.Context(), id, true)
		if client := h.xrayHolder.Get(); client != nil {
			client.AddUser(r.Context(), profile.UUID, profile.UUID)
		}
		log.Printf("Admin: profile %s reactivated after traffic reset", profile.UUID)
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
