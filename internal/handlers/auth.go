package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"xray-panel/internal/email"
	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
)

type AuthHandler struct {
	users        *models.UserStore
	auth         *middleware.AuthMiddleware
	renderer     *Renderer
	mailer       *email.Sender // nil если SMTP не настроен
	baseURL      string
	resetLimiter *middleware.RateLimiter // 3 запроса/час на IP для /forgot
}

// resetLimit и resetWindow — текстовые константы для сноски на форме.
// Должны соответствовать параметрам RateLimiter, переданному в NewAuthHandler.
const (
	ResetLimitMax    = 3
	ResetLimitWindow = "час"
)

func NewAuthHandler(users *models.UserStore, auth *middleware.AuthMiddleware, renderer *Renderer, mailer *email.Sender, baseURL string, resetLimiter *middleware.RateLimiter) *AuthHandler {
	return &AuthHandler{users: users, auth: auth, renderer: renderer, mailer: mailer, baseURL: baseURL, resetLimiter: resetLimiter}
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

	// Если email не подтверждён — на страницу ожидания
	if !user.EmailVerified {
		http.Redirect(w, r, "/verify-pending", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *AuthHandler) RegisterPage(w http.ResponseWriter, r *http.Request) {
	h.renderer.Render(w, "register.html", nil)
}

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	emailAddr := r.FormValue("email")
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

	user, token, err := h.users.Create(r.Context(), username, emailAddr, password)
	if err != nil {
		log.Printf("Register failed: %v", err)
		h.renderer.Render(w, "register.html", map[string]string{
			"Error": "Пользователь с таким логином или email уже существует",
		})
		return
	}

	// Отправляем письмо верификации
	if h.mailer != nil {
		go func() {
			if err := h.mailer.SendVerification(emailAddr, token, h.baseURL); err != nil {
				log.Printf("Failed to send verification email to %s: %v", emailAddr, err)
			}
		}()
	} else {
		// SMTP не настроен — логируем токен для ручной верификации
		log.Printf("SMTP not configured. Verify URL: %s/verify?token=%s", h.baseURL, token)
	}

	h.auth.SetSession(w, user.ID)
	http.Redirect(w, r, "/verify-pending", http.StatusSeeOther)
}

// VerifyPendingPage — страница «проверьте почту»
func (h *AuthHandler) VerifyPendingPage(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if user.EmailVerified {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.renderer.Render(w, "verify_pending.html", map[string]any{
		"User": user,
	})
}

// ResendVerification — повторная отправка письма
func (h *AuthHandler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if user.EmailVerified {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	token, emailAddr, err := h.users.RegenerateVerifyToken(r.Context(), user.ID)
	if err != nil {
		log.Printf("Resend verification failed for user %d: %v", user.ID, err)
		h.renderer.Render(w, "verify_pending.html", map[string]any{
			"User":  user,
			"Error": "Не удалось отправить письмо, попробуйте позже",
		})
		return
	}

	if h.mailer != nil {
		go func() {
			if err := h.mailer.SendVerification(emailAddr, token, h.baseURL); err != nil {
				log.Printf("Failed to resend verification to %s: %v", emailAddr, err)
			}
		}()
	} else {
		log.Printf("SMTP not configured. Verify URL: %s/verify?token=%s", h.baseURL, token)
	}

	h.renderer.Render(w, "verify_pending.html", map[string]any{
		"User":    user,
		"Success": "Письмо отправлено повторно",
	})
}

// VerifyEmail — GET /verify?token=xxx
func (h *AuthHandler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		h.renderer.Render(w, "verify_result.html", map[string]any{
			"Error": "Токен не указан",
		})
		return
	}

	user, err := h.users.VerifyEmail(r.Context(), token)
	if err != nil {
		log.Printf("Email verification failed: %v", err)
		h.renderer.Render(w, "verify_result.html", map[string]any{
			"Error": "Недействительная или просроченная ссылка",
		})
		return
	}

	// Устанавливаем сессию (если не было)
	h.auth.SetSession(w, user.ID)

	h.renderer.Render(w, "verify_result.html", map[string]any{
		"Success": true,
		"User":    user,
	})
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	h.auth.ClearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ForgotPasswordPage — GET-форма запроса восстановления (логин или email).
func (h *AuthHandler) ForgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	h.renderer.Render(w, "forgot_password.html", map[string]any{
		"LimitMax":    ResetLimitMax,
		"LimitWindow": ResetLimitWindow,
	})
}

// ForgotPassword — POST: отправка письма со ссылкой.
// Ответ всегда одинаковый (даже если юзера нет), чтобы не раскрывать наличие
// аккаунта в системе.
func (h *AuthHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	loginOrEmail := strings.TrimSpace(r.FormValue("login_or_email"))

	if h.resetLimiter != nil && !h.resetLimiter.Allow(r.RemoteAddr) {
		h.renderer.Render(w, "forgot_password.html", map[string]any{
			"LimitMax":     ResetLimitMax,
			"LimitWindow":  ResetLimitWindow,
			"Error":        fmt.Sprintf("Превышен лимит — не более %d запросов в %s. Попробуйте позже.", ResetLimitMax, ResetLimitWindow),
			"LoginOrEmail": loginOrEmail,
		})
		return
	}

	if loginOrEmail == "" {
		h.renderer.Render(w, "forgot_password.html", map[string]any{
			"LimitMax":    ResetLimitMax,
			"LimitWindow": ResetLimitWindow,
			"Error":       "Укажите логин или email",
		})
		return
	}

	userID, emailAddr, token, err := h.users.CreateResetToken(r.Context(), loginOrEmail)
	if err != nil {
		log.Printf("CreateResetToken failed for %q: %v", loginOrEmail, err)
	}
	if userID != nil && token != "" {
		if h.mailer != nil {
			go func() {
				if sendErr := h.mailer.SendPasswordReset(emailAddr, token, h.baseURL); sendErr != nil {
					log.Printf("Failed to send password reset to %s: %v", emailAddr, sendErr)
				}
			}()
		} else {
			log.Printf("SMTP not configured. Password reset URL: %s/reset?token=%s", h.baseURL, token)
		}
	}

	h.renderer.Render(w, "forgot_password.html", map[string]any{
		"LimitMax":    ResetLimitMax,
		"LimitWindow": ResetLimitWindow,
		"Success":     "Если такой аккаунт существует, мы отправили письмо со ссылкой для восстановления. Проверьте почту (в т.ч. папку «Спам»). Ссылка действует 1 час.",
	})
}

// ResetPasswordPage — GET /reset?token=xxx, форма ввода нового пароля.
func (h *AuthHandler) ResetPasswordPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		h.renderer.Render(w, "reset_password.html", map[string]any{
			"Error": "Токен не указан",
		})
		return
	}
	h.renderer.Render(w, "reset_password.html", map[string]any{
		"Token": token,
	})
}

// ResetPassword — POST: проверка и установка нового пароля.
func (h *AuthHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	password := r.FormValue("password")
	passwordConfirm := r.FormValue("password_confirm")

	if token == "" {
		h.renderer.Render(w, "reset_password.html", map[string]any{
			"Error": "Токен не указан",
		})
		return
	}
	if password != passwordConfirm {
		h.renderer.Render(w, "reset_password.html", map[string]any{
			"Token": token,
			"Error": "Пароли не совпадают",
		})
		return
	}
	if len(password) < 6 {
		h.renderer.Render(w, "reset_password.html", map[string]any{
			"Token": token,
			"Error": "Пароль минимум 6 символов",
		})
		return
	}

	user, err := h.users.ResetPassword(r.Context(), token, password)
	if err != nil {
		log.Printf("Password reset failed: %v", err)
		h.renderer.Render(w, "reset_password.html", map[string]any{
			"Error": "Недействительная или просроченная ссылка. Запросите восстановление заново.",
		})
		return
	}

	// Логиним юзера и ведём в дашборд (или на verify-pending, если email не подтверждён).
	h.auth.SetSession(w, user.ID)
	if !user.EmailVerified {
		http.Redirect(w, r, "/verify-pending", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
