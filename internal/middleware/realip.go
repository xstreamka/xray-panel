package middleware

import (
	"log"
	"net"
	"net/http"
	"strings"
)

// RealIP подменяет r.RemoteAddr на честный клиентский IP (без порта),
// доверяя X-Forwarded-For / X-Real-IP только если соединение пришло от
// proxy из trustedNets. Иначе используется peer-IP из r.RemoteAddr —
// чтобы клиент из интернета не мог обойти rate-limit подделкой заголовка.
//
// trustedNets парсится из cfg.TrustedProxies (CIDR через запятую).
// Пустой список = заголовкам не доверяем никогда.
type RealIP struct {
	trustedNets []*net.IPNet
}

func NewRealIP(cidrs string) *RealIP {
	r := &RealIP{}
	for _, raw := range strings.Split(cidrs, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		// Принимаем как голый IP, так и CIDR.
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil {
				if ip.To4() != nil {
					s += "/32"
				} else {
					s += "/128"
				}
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			log.Printf("RealIP: bad TRUSTED_PROXIES entry %q: %v", raw, err)
			continue
		}
		r.trustedNets = append(r.trustedNets, n)
	}
	return r
}

func (rip *RealIP) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = rip.resolve(r)
		next.ServeHTTP(w, r)
	})
}

func (rip *RealIP) resolve(r *http.Request) string {
	peer := stripPort(r.RemoteAddr)
	if !rip.peerTrusted(peer) {
		return peer
	}
	// Доверяем заголовкам. X-Real-IP проще и однозначнее, поэтому пробуем первым.
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		if ip := net.ParseIP(v); ip != nil {
			return ip.String()
		}
	}
	// X-Forwarded-For: берём самый правый IP, который НЕ из trusted-сети —
	// это и есть реальный клиент перед нашей цепочкой прокси.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(parts[i])
			ip := net.ParseIP(candidate)
			if ip == nil {
				continue
			}
			if !rip.ipTrusted(ip) {
				return ip.String()
			}
		}
	}
	return peer
}

func (rip *RealIP) peerTrusted(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return rip.ipTrusted(ip)
}

func (rip *RealIP) ipTrusted(ip net.IP) bool {
	for _, n := range rip.trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func stripPort(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
