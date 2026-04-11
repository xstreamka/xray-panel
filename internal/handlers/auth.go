package handlers

import (
	"log"
	"net/http"

	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
)

type AuthHandler struct {
	users    *models.UserStore
	auth     *middleware.AuthMiddleware
	renderer *Renderer
}

func NewAuthHandler(users *models.UserStore, auth *middleware.AuthMiddleware, renderer *Renderer) *AuthHandler {
	return &AuthHandler{users: users, auth: auth, renderer: renderer}
}

func (h *AuthHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	h.renderer.Render(w, "login.html", nil)
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := h.users.Authenticate(r.Context(), username, password)
	if err != nil {
		log.Printf("Login failed for %s: %v", username, err)
		h.renderer.Render(w, "login.html", map[string]string{
			"Error": "Неверный логин или пароль",
		})
		return
	}

	h.auth.SetSession(w, user.ID)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *AuthHandler) RegisterPage(w http.ResponseWriter, r *http.Request) {
	h.renderer.Render(w, "register.html", nil)
}

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	email := r.FormValue("email")
	password := r.FormValue("password")
	passwordConfirm := r.FormValue("password_confirm")

	if password != passwordConfirm {
		h.renderer.Render(w, "register.html", map[string]string{
			"Error": "Пароли не совпадают",
		})
		return
	}

	if len(password) < 6 {
		h.renderer.Render(w, "register.html", map[string]string{
			"Error": "Пароль минимум 6 символов",
		})
		return
	}

	user, err := h.users.Create(r.Context(), username, email, password)
	if err != nil {
		log.Printf("Register failed: %v", err)
		h.renderer.Render(w, "register.html", map[string]string{
			"Error": "Пользователь с таким логином или email уже существует",
		})
		return
	}

	h.auth.SetSession(w, user.ID)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	h.auth.ClearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
