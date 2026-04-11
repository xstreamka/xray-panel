package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xray-panel/internal/models"
)

type contextKey string

const userContextKey contextKey = "user"

type AuthMiddleware struct {
	userStore *models.UserStore
	secretKey string
}

func NewAuthMiddleware(userStore *models.UserStore, secretKey string) *AuthMiddleware {
	return &AuthMiddleware{userStore: userStore, secretKey: secretKey}
}

// RequireAuth — middleware для защищённых роутов
func (m *AuthMiddleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := m.getUserFromCookie(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAdmin — middleware для админских роутов
func (m *AuthMiddleware) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil || !user.IsAdmin {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetSession — устанавливает cookie с подписанным user ID
func (m *AuthMiddleware) SetSession(w http.ResponseWriter, userID int) {
	value := fmt.Sprintf("%d|%d", userID, time.Now().Unix())
	sig := m.sign(value)
	cookie := &http.Cookie{
		Name:     "session",
		Value:    value + "|" + sig,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, //TODO: поменять на true когда будет за nginx с HTTPS
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 30, // 30 дней
	}
	http.SetCookie(w, cookie)
}

func (m *AuthMiddleware) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   "session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}

func (m *AuthMiddleware) getUserFromCookie(r *http.Request) (*models.User, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil, err
	}

	parts := strings.SplitN(cookie.Value, "|", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid session")
	}

	value := parts[0] + "|" + parts[1]
	sig := parts[2]

	if !m.verify(value, sig) {
		return nil, fmt.Errorf("invalid signature")
	}

	userID, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, err
	}

	return m.userStore.GetByID(r.Context(), userID)
}

func (m *AuthMiddleware) sign(data string) string {
	mac := hmac.New(sha256.New, []byte(m.secretKey))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *AuthMiddleware) verify(data, signature string) bool {
	expected := m.sign(data)
	return hmac.Equal([]byte(expected), []byte(signature))
}

func UserFromContext(ctx context.Context) *models.User {
	user, _ := ctx.Value(userContextKey).(*models.User)
	return user
}
