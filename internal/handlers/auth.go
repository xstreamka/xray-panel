package handlers

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"

	"xray-panel/internal/email"
	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
)

// msgUserDeactivated — текст для формы входа и редиректа из middleware,
// когда учётка помечена is_active=FALSE. Единое место, чтобы формулировка
// совпадала во всех точках. Тип template.HTML, т.к. внутри есть <br>,
// который html/template иначе бы заэкранировал в &lt;br&gt;.
const msgUserDeactivated template.HTML = "Пользователь выключен.<br>Вход невозможен, обратитесь к администратору."

type AuthHandler struct {
	users        *models.UserStore
	invites      *models.InviteStore
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

func NewAuthHandler(users *models.UserStore, invites *models.InviteStore, auth *middleware.AuthMiddleware, renderer *Renderer, mailer *email.Sender, baseURL string, resetLimiter *middleware.RateLimiter) *AuthHandler {
	return &AuthHandler{users: users, invites: invites, auth: auth, renderer: renderer, mailer: mailer, baseURL: baseURL, resetLimiter: resetLimiter}
}

func (h *AuthHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{}
	// ?deactivated=1 — юзер был выкинут из сессии middleware'ом из-за
	// is_active=FALSE. Показываем осмысленное сообщение.
	if r.URL.Query().Get("deactivated") == "1" {
		data["Error"] = msgUserDeactivated
	}
	// Режим регистрации: шаблон показывает ссылку «Регистрация» только
	// когда открытая регистрация действительно доступна.
	mode, _ := h.invites.GetRegistrationMode(r.Context())
	data["ShowRegisterLink"] = (mode == models.RegModeOpen || mode == models.RegModeBoth)
	h.renderer.Render(w, "login.html", data)
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := h.users.Authenticate(r.Context(), username, password)
	if err != nil {
		log.Printf("Login failed for %s: %v", username, err)
		// any: сюда кладётся либо string (экранируется автоматически),
		// либо template.HTML для сообщений с разметкой вроде <br>.
		var errMsg any = "Неверный логин или пароль"
		if errors.Is(err, models.ErrUserDeactivated) {
			errMsg = msgUserDeactivated
		}
		mode, _ := h.invites.GetRegistrationMode(r.Context())
		h.renderer.Render(w, "login.html", map[string]any{
			"Error":            errMsg,
			"ShowRegisterLink": mode == models.RegModeOpen || mode == models.RegModeBoth,
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

// resolveRegistrationAccess — разбор режима регистрации + кода инвайта в три
// состояния:
//   - allow=true, showForm=true  → рендерим форму (с invite или без)
//   - allow=false, showForm=false → рендерим «закрыто» с нужным текстом в stateMsg
//
// invite != nil означает что код валиден и нужно будет привязать юзера к нему
// при успешной регистрации. stateKind — машинно-читаемый ярлык для шаблона
// ("disabled" / "invite_only" / "invite_invalid"), чтобы шаблон мог выбрать
// формулировку самостоятельно.
type registrationAccess struct {
	Allow     bool
	Mode      string
	Invite    *models.Invite
	StateKind string // "", "disabled", "invite_only", "invite_invalid"
}

func (h *AuthHandler) resolveRegistrationAccess(r *http.Request) registrationAccess {
	mode, _ := h.invites.GetRegistrationMode(r.Context())
	code := strings.TrimSpace(r.URL.Query().Get("invite"))
	// На POST код приходит скрытым полем формы — подхватим и его.
	if code == "" {
		code = strings.TrimSpace(r.FormValue("invite"))
	}

	switch mode {
	case models.RegModeDisabled:
		return registrationAccess{Mode: mode, StateKind: "disabled"}

	case models.RegModeInviteOnly:
		if code == "" {
			return registrationAccess{Mode: mode, StateKind: "invite_only"}
		}
		inv, err := h.invites.GetByCode(r.Context(), code)
		if err != nil {
			return registrationAccess{Mode: mode, StateKind: "invite_invalid"}
		}
		return registrationAccess{Allow: true, Mode: mode, Invite: inv}

	case models.RegModeBoth:
		if code == "" {
			return registrationAccess{Allow: true, Mode: mode}
		}
		inv, err := h.invites.GetByCode(r.Context(), code)
		if err != nil {
			// both + битый код — показываем как invalid: админ хотел,
			// чтобы по сломанной ссылке не падали в «просто open»-форму,
			// иначе метрика по инвайту рассыплется.
			return registrationAccess{Mode: mode, StateKind: "invite_invalid"}
		}
		return registrationAccess{Allow: true, Mode: mode, Invite: inv}

	default: // open
		if code != "" {
			// В open-режиме код тоже учитываем, если он валиден — так у
			// админа не ломается аналитика при смене режима туда-обратно.
			if inv, err := h.invites.GetByCode(r.Context(), code); err == nil {
				return registrationAccess{Allow: true, Mode: mode, Invite: inv}
			}
		}
		return registrationAccess{Allow: true, Mode: mode}
	}
}

func (h *AuthHandler) RegisterPage(w http.ResponseWriter, r *http.Request) {
	acc := h.resolveRegistrationAccess(r)

	data := map[string]any{
		"Mode":      acc.Mode,
		"StateKind": acc.StateKind,
		"Allow":     acc.Allow,
	}
	if acc.Invite != nil {
		data["InviteCode"] = acc.Invite.Code
		// Клики инкрементим только на GET: на POST юзер уже кликнул ранее.
		if err := h.invites.IncrementClicks(r.Context(), acc.Invite.ID); err != nil {
			log.Printf("Invite %d: increment clicks failed: %v", acc.Invite.ID, err)
		}
	}
	h.renderer.Render(w, "register.html", data)
}

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	acc := h.resolveRegistrationAccess(r)

	// Базовый набор полей для повторной отрисовки с ошибкой, чтобы юзер
	// не терял введённое.
	renderWithErr := func(msg string) {
		data := map[string]any{
			"Mode":      acc.Mode,
			"StateKind": acc.StateKind,
			"Allow":     acc.Allow,
			"Error":     msg,
			"Username":  r.FormValue("username"),
			"EmailVal":  r.FormValue("email"),
		}
		if acc.Invite != nil {
			data["InviteCode"] = acc.Invite.Code
		}
		h.renderer.Render(w, "register.html", data)
	}

	if !acc.Allow {
		// Попытка POST в закрытом режиме — просто перерисовываем страницу
		// в её текущем «не-форма» состоянии.
		h.renderer.Render(w, "register.html", map[string]any{
			"Mode":      acc.Mode,
			"StateKind": acc.StateKind,
			"Allow":     false,
		})
		return
	}

	username := r.FormValue("username")
	emailAddr := r.FormValue("email")
	password := r.FormValue("password")
	passwordConfirm := r.FormValue("password_confirm")

	if password != passwordConfirm {
		renderWithErr("Пароли не совпадают")
		return
	}
	if len(password) < 6 {
		renderWithErr("Пароль минимум 6 символов")
		return
	}

	var inviteID *int
	if acc.Invite != nil {
		inviteID = &acc.Invite.ID
	}

	user, token, err := h.users.Create(r.Context(), username, emailAddr, password, inviteID)
	if err != nil {
		log.Printf("Register failed: %v", err)
		renderWithErr("Пользователь с таким логином или email уже существует")
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
// Для отсутствующего юзера отвечаем универсально, чтобы не раскрывать наличие
// аккаунта. Для выключенного (is_active=FALSE) — по просьбе админки явно
// сообщаем «пользователь выключен». Это сознательный компромисс UX ↔ privacy.
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
	if errors.Is(err, models.ErrUserDeactivated) {
		h.renderer.Render(w, "forgot_password.html", map[string]any{
			"LimitMax":     ResetLimitMax,
			"LimitWindow":  ResetLimitWindow,
			"Error":        template.HTML("Пользователь выключен.<br>Восстановление пароля невозможно, обратитесь к администратору."),
			"LoginOrEmail": loginOrEmail,
		})
		return
	}
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
