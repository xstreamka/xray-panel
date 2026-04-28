package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// SecurityHeaders проставляет базовые security-заголовки на каждый ответ.
// HSTS включается только на HTTPS, чтобы случайно не сломать локальную разработку
// по http://. CSP сознательно строгая (no inline scripts) — весь JS лежит в
// /static/js/, инлайнового скрипта в шаблонах не должно быть.
//
// formActionExtra — дополнительные origin'ы для form-action (через пробел или
// запятую). Нужно, например, для редиректа /pay/checkout на внешний pay-service:
// CSP form-action охватывает и редиректы из same-origin form, поэтому без добавления
// домена pay-service кнопка «Оплатить» блокируется браузером.
func SecurityHeaders(baseURL string, formActionExtra ...string) func(http.Handler) http.Handler {
	isHTTPS := strings.HasPrefix(strings.ToLower(baseURL), "https://")
	formAction := "'self'"
	for _, raw := range formActionExtra {
		if origin := parseOrigin(raw); origin != "" {
			formAction += " " + origin
		}
	}
	csp := "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"form-action " + formAction + "; " +
		"frame-ancestors 'none'; " +
		"base-uri 'self'"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
			h.Set("Content-Security-Policy", csp)
			if isHTTPS {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// parseOrigin вытаскивает scheme://host[:port] из произвольной строки-URL.
// Пустую строку и нераспознанные значения молча проглатывает — лучше CSP без
// дополнительного origin, чем сломанная политика из-за опечатки в env.
func parseOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
