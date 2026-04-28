package middleware

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// Synchronizer-token CSRF: токен живёт в HttpOnly cookie + дублируется в hidden-поле
// или X-CSRF-Token заголовке. На POST/PUT/PATCH/DELETE мы сравниваем cookie с тем,
// что прислал клиент. Атакующий с другого origin не может ни прочитать HttpOnly
// cookie, ни узнать её значение через CSP-блокированный JS — поэтому подделать
// форму не сможет. SameSite=Lax — дополнительный слой защиты.
const (
	csrfCookieName = "csrf_token"
	csrfHeaderName = "X-CSRF-Token"
	csrfFormField  = "csrf_token"
	csrfMaxAge     = 86400 * 30
)

type csrfCtxKey struct{}

type CSRFMiddleware struct {
	secureCookie bool
}

func NewCSRFMiddleware(baseURL string) *CSRFMiddleware {
	return &CSRFMiddleware{
		secureCookie: strings.HasPrefix(strings.ToLower(baseURL), "https://"),
	}
}

func (m *CSRFMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := readCSRFCookie(r)
		if token == "" {
			token = generateCSRFToken()
			m.setCookie(w, token)
		}

		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			sent := r.Header.Get(csrfHeaderName)
			if sent == "" {
				// PostFormValue сам вызывает ParseForm/ParseMultipartForm.
				sent = r.PostFormValue(csrfFormField)
			}
			if sent == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 {
				http.Error(w, "CSRF token mismatch", http.StatusForbidden)
				return
			}
		}

		ctx := context.WithValue(r.Context(), csrfCtxKey{}, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (m *CSRFMiddleware) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   csrfMaxAge,
	})
}

func readCSRFCookie(r *http.Request) string {
	c, err := r.Cookie(csrfCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func generateCSRFToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand отказывает только при системной деградации — продолжать
		// с предсказуемым токеном опаснее, чем уронить запрос.
		panic("csrf: rand.Read failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// CSRFTokenFromContext возвращает токен, проставленный middleware'ом.
// Используется Renderer'ом для прокидки в шаблоны (поле .CSRFToken).
func CSRFTokenFromContext(ctx context.Context) string {
	t, _ := ctx.Value(csrfCtxKey{}).(string)
	return t
}
