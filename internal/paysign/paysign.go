// Package paysign реализует HMAC-SHA256 подпись параметров redirect-а на pay-service.
// Алгоритм ДОЛЖЕН быть идентичен pay-service/internal/handlers/payment.go:
// "sig" пропускается, остальные ключи сортируются, строка вида "k1=v1&k2=v2&..."
// подписывается HMAC-SHA256 с общим секретом (WEBHOOK_SECRET).
package paysign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Sign подписывает map параметров.
func Sign(params map[string]string, secret string) string {
	canonical := buildCanonical(params)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyBody проверяет HMAC-SHA256 от raw-тела webhook (pay-service → panel).
// Использует тот же секрет. Применяется в webhook-receiver.
func VerifyBody(body []byte, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

func buildCanonical(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "sig" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "&")
}
