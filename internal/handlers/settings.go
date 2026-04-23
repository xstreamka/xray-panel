package handlers

import (
	"log"
	"net/http"

	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
)

type SettingsHandler struct {
	users    *models.UserStore
	renderer *Renderer
}

func NewSettingsHandler(users *models.UserStore, renderer *Renderer) *SettingsHandler {
	return &SettingsHandler{users: users, renderer: renderer}
}

// Index — страница «Настройки» с галочками уведомлений.
func (h *SettingsHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	h.renderer.Render(w, "settings.html", map[string]any{
		"Active": "settings",
		"User":   user,
	})
}

// Save — POST: записать галочки. Чекбокс отсутствует в форме, если снят,
// поэтому читаем через FormValue("...") == "on".
func (h *SettingsHandler) Save(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	topup := r.FormValue("notify_topup") == "on"
	expiration := r.FormValue("notify_expiration") == "on"
	block := r.FormValue("notify_block") == "on"
	trafficLow := r.FormValue("notify_traffic_low") == "on"

	if err := h.users.UpdateNotificationPrefs(r.Context(), user.ID,
		topup, expiration, block, trafficLow); err != nil {
		log.Printf("Settings: update prefs user=%d: %v", user.ID, err)
		http.Error(w, "Не удалось сохранить настройки", http.StatusInternalServerError)
		return
	}

	// После сохранения перезапрашиваем юзера, чтобы отрисовать свежие значения,
	// а не то, что лежало в контексте из RequireAuth (оно уже устарело).
	u, err := h.users.GetByID(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	h.renderer.Render(w, "settings.html", map[string]any{
		"Active":  "settings",
		"User":    u,
		"Success": "Настройки сохранены",
	})
}
