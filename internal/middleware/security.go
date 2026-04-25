package middleware

import (
	"net/http"
	"strings"
)

// SecurityHeaders проставляет базовые security-заголовки на каждый ответ.
// HSTS включается только на HTTPS, чтобы случайно не сломать локальную разработку
// по http://. CSP сознательно строгая (no inline scripts) — весь JS лежит в
// /static/js/, инлайнового скрипта в шаблонах не должно быть.
func SecurityHeaders(baseURL string) func(http.Handler) http.Handler {
	isHTTPS := strings.HasPrefix(strings.ToLower(baseURL), "https://")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
			h.Set("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src 'self'; "+
					"style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; "+
					"font-src 'self'; "+
					"connect-src 'self'; "+
					"form-action 'self'; "+
					"frame-ancestors 'none'; "+
					"base-uri 'self'")
			if isHTTPS {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
