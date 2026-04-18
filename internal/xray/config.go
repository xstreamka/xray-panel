package xray

import (
	"encoding/json"
	"fmt"
	"os"

	"xray-panel/internal/config"
)

// GenerateConfig создаёт config.json для Xray с включённым API, stats и policy
func GenerateConfig(cfg *config.Config, activeUUIDs []string, outputPath string) error {
	// Пустой clients — пользователями управляем исключительно через gRPC API
	// (syncUsersToXray добавляет активных при старте через AddUser).
	// Это критично: RemoveUser работает ТОЛЬКО для API-добавленных пользователей,
	// статических из конфига оно не трогает!
	clients := []map[string]any{}
	_ = activeUUIDs // используются только для логирования в main.g
	for _, uuid := range activeUUIDs {
		clients = append(clients, map[string]any{
			"id":    uuid,
			"email": uuid,
		})
	}

	xrayConfig := map[string]any{
		// Логирование — видим какой outbound выбран для каждого запроса
		"log": map[string]any{
			"loglevel": "warning",
			"access":   "/var/log/xray/access.log",
			"error":    "/var/log/xray/error.log",
		},

		// DNS — Xray резолвит домены сам для routing
		// Без этой секции IPIfNonMatch не работает!
		"dns": map[string]any{
			"servers": []any{
				// РФ домены через Яндекс DNS
				map[string]any{
					"address": "77.88.8.8",
					"port":    53,
					"domains": []string{
						"geosite:category-ru",
						"domain:ru",
						"domain:su",
						"domain:xn--p1ai",
					},
				},
				// Остальное
				"8.8.8.8",
				"1.1.1.1",
			},
		},

		// API
		"api": map[string]any{
			"tag": "api",
			"services": []string{
				"HandlerService",
				"StatsService",
			},
		},

		"stats": map[string]any{},

		"policy": map[string]any{
			"levels": map[string]any{
				"0": map[string]any{
					"statsUserUplink":   true,
					"statsUserDownlink": true,
					"statsUserOnline":   true,
				},
			},
			"system": map[string]any{
				"statsInboundUplink":    true,
				"statsInboundDownlink":  true,
				"statsOutboundUplink":   true,
				"statsOutboundDownlink": true,
			},
		},

		// Inbounds
		"inbounds": []map[string]any{
			{
				"tag":      "api-in",
				"listen":   "127.0.0.1",
				"port":     10085,
				"protocol": "dokodemo-door",
				"settings": map[string]any{
					"address": "127.0.0.1",
				},
			},
			{
				"tag":      cfg.XrayInboundTag,
				"listen":   "0.0.0.0",
				"port":     443,
				"protocol": "vless",
				"settings": map[string]any{
					"clients":    clients,
					"decryption": "none",
				},
				"streamSettings": map[string]any{
					"network":  "tcp",
					"security": "reality",
					"realitySettings": map[string]any{
						"show":        false,
						"dest":        cfg.RealityDest,
						"xver":        0,
						"serverNames": cfg.RealityServerNames,
						"privateKey":  cfg.RealityPrivateKey,
						"shortIds":    []string{cfg.RealityShortID},
					},
				},
				"sniffing": map[string]any{
					"enabled":      true,
					"destOverride": []string{"http", "tls", "quic"},
					"routeOnly":    true, // только для роутинга, не трогаем destination
				},
			},
		},

		// Outbounds
		"outbounds": []map[string]any{
			// Амстердам — default (первый)
			{
				"tag":      "proxy-out",
				"protocol": "vless",
				"settings": map[string]any{
					"vnext": []map[string]any{
						{
							"address": cfg.AmsterdamAddr,
							"port":    cfg.AmsterdamPort,
							"users": []map[string]any{
								{
									"id":         cfg.AmsterdamUUID,
									"encryption": "none",
								},
							},
						},
					},
				},
				"streamSettings": map[string]any{
					"network":  "tcp",
					"security": "reality",
					"realitySettings": map[string]any{
						"serverName":  cfg.AmsterdamSNI,
						"fingerprint": "chrome",
						"publicKey":   cfg.AmsterdamPublicKey,
						"shortId":     cfg.AmsterdamShortID,
					},
				},
			},
			// Direct — выход с ЯО
			{
				"tag":      "direct",
				"protocol": "freedom",
				"settings": map[string]any{
					"domainStrategy": "UseIPv4",
				},
			},
		},

		// Routing
		"routing": map[string]any{
			"domainStrategy": "IPIfNonMatch",
			"domainMatcher":  "hybrid",
			"rules": []map[string]any{
				// 1. API
				{
					"type":        "field",
					"inboundTag":  []string{"api-in"},
					"outboundTag": "api",
				},
				// 2. РФ домены → direct
				{
					"type": "field",
					"domain": []string{
						"geosite:category-ru",
						"domain:ru",
						"domain:su",
						"domain:xn--p1ai",
						"domain:yandex.com",
						"domain:yandex.net",
						"domain:yastatic.net",
						"domain:vk.com",
						"domain:vkontakte.ru",
						"domain:vk.me",
						"domain:mail.ru",
						"domain:ok.ru",
						"domain:sberbank.ru",
						"domain:sber.ru",
						"domain:tinkoff.ru",
						"domain:gosuslugi.ru",
						"domain:nalog.gov.ru",
						"domain:mos.ru",
					},
					"outboundTag": "direct",
				},
				// 3. РФ IP → direct (подстраховка через IPIfNonMatch)
				{
					"type":        "field",
					"ip":          []string{"geoip:ru"},
					"outboundTag": "direct",
				},
				// 4. Приватные сети
				{
					"type":        "field",
					"ip":          []string{"geoip:private"},
					"outboundTag": "direct",
				},
				// 5. Всё остальное → Амстердам
				{
					"type":        "field",
					"inboundTag":  []string{cfg.XrayInboundTag},
					"outboundTag": "proxy-out",
				},
			},
		},
	}

	data, err := json.MarshalIndent(xrayConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	// Создаём директорию для логов (не критично если не получится)
	os.MkdirAll("/var/log/xray", 0755)

	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}
