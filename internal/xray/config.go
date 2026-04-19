package xray

import (
	"encoding/json"
	"fmt"
	"os"

	"xray-panel/internal/config"
)

// GenerateConfig создаёт config.json для Xray с включённым API, stats и policy.
// Базируется на проверенном вручную рабочем конфиге (5.178.85.153 → Amsterdam).
//
// ВАЖНО про статистику:
// Flow xtls-rprx-vision использует splice (sendfile в ядре), из-за чего
// stats-счётчики пользователей недосчитывают значительную часть трафика —
// через userspace проходят только control-пакеты. Это компромисс ради
// скорости и обхода DPI. Для точного биллинга использовать nftables counters.
func GenerateConfig(cfg *config.Config, activeUUIDs []string, outputPath string) error {
	// Пустой clients — пользователями управляем исключительно через gRPC API
	// (syncUsersToXray добавляет активных при старте через AddUser).
	// Это критично: RemoveUser работает ТОЛЬКО для API-добавленных пользователей,
	// статических из конфига оно не трогает!
	clients := []map[string]any{}
	_ = activeUUIDs // используются только для логирования в main.go
	for _, uuid := range activeUUIDs {
		clients = append(clients, map[string]any{
			"id":    uuid,
			"email": uuid,
			"flow":  "xtls-rprx-vision",
		})
	}

	xrayConfig := map[string]any{
		// Логирование
		"log": map[string]any{
			"loglevel": "warning",
			"access":   "/var/log/xray/access.log",
			"error":    "/var/log/xray/error.log",
		},

		// DNS — Xray резолвит домены сам для routing.
		// Без этой секции IPIfNonMatch не работает корректно.
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
				"tag":      cfg.XrayInboundTag, // "vless-in"
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
						"dest":        cfg.RealityDest,
						"serverNames": []string{cfg.RealityServerName},
						"privateKey":  cfg.RealityPrivateKey,
						"shortIds":    []string{cfg.RealityShortID},
					},
				},
				"sniffing": map[string]any{
					"enabled":      true,
					"destOverride": []string{"http", "tls", "quic"},
					"routeOnly":    true, // только для роутинга, destination не подменяем (важно для Vision)
				},
			},
		},

		// Outbounds
		"outbounds": []map[string]any{
			// Амстердам — зарубежное ходит сюда
			{
				"tag":      "proxy",
				"protocol": "vless",
				"settings": map[string]any{
					"vnext": []map[string]any{
						{
							"address": cfg.AmsterdamAddr,
							"port":    cfg.AmsterdamPort,
							"users": []map[string]any{
								{
									"id":         cfg.AmsterdamUUID,
									"flow":       "xtls-rprx-vision",
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
			// Direct — выход с этого сервера (РФ)
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
			"domainMatcher":  "hybrid",
			"domainStrategy": "IPIfNonMatch",
			"rules": []map[string]any{
				// 1. Анти-петля: dial до самого Амстердама всегда direct,
				//    иначе outbound proxy попадёт сам в себя.
				{
					"type":        "field",
					"ip":          []string{cfg.AmsterdamAddr},
					"outboundTag": "direct",
				},
				// 2. API
				{
					"type":        "field",
					"inboundTag":  []string{"api-in"},
					"outboundTag": "api",
				},
				// 3. РФ домены → direct
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
				// 4. РФ IP → direct (подстраховка через IPIfNonMatch)
				{
					"type":        "field",
					"ip":          []string{"geoip:ru"},
					"outboundTag": "direct",
				},
				// 5. Приватные сети
				{
					"type":        "field",
					"ip":          []string{"geoip:private"},
					"outboundTag": "direct",
				},
				// 6. Всё остальное с vless-in → Амстердам
				{
					"type":        "field",
					"inboundTag":  []string{cfg.XrayInboundTag},
					"outboundTag": "proxy",
				},
			},
		},
	}

	data, err := json.MarshalIndent(xrayConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	// Создаём директорию для логов (не критично если не получится)
	_ = os.MkdirAll("/var/log/xray", 0755)

	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}
