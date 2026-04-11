package handlers

import (
	"fmt"
	"log"
	"net/http"

	"xray-panel/internal/config"
	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
	"xray-panel/internal/xray"

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

func (h *DashboardHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	profiles, err := h.profiles.GetByUserID(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	type profileView struct {
		models.VPNProfile
		VlessURI string
	}

	var views []profileView
	for _, p := range profiles {
		views = append(views, profileView{
			VPNProfile: p,
			VlessURI:   h.buildVlessURI(p.UUID, p.Name),
		})
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
